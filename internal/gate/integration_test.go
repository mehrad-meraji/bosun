//go:build integration

package gate_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/registry"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const repo = "localhost:5055/bosun-test/app"

const (
	v1     = "FROM busybox:1.36\nRUN echo v1 > /version\nCMD [\"sleep\", \"3600\"]\n"
	v2     = "FROM busybox:1.36\nRUN echo v2 > /version\nCMD [\"sleep\", \"3600\"]\n"
	broken = "FROM busybox:1.36\nRUN echo broken > /version\nCMD [\"false\"]\n"
	newCmd = "FROM busybox:1.36\nRUN echo v2 > /version\nCMD [\"sleep\", \"7200\"]\n"
	// noisy fails like broken, but says why first, on stderr and with no TTY.
	noisy = "FROM busybox:1.36\nRUN echo noisy > /version\nCMD [\"sh\", \"-c\", \"echo 'boom: no such table' >&2; exit 1\"]\n"
)

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

var buildTagSeq atomic.Int64

// push builds dockerfile, pushes it as repo:latest, drops the local tags so
// the gate must really pull, and returns the registry digest.
//
// It builds under a unique tag and then retags: on OrbStack (containerd image
// store), `docker build -t repo:latest` onto a tag that a running container's
// image just lost garbage-collects that in-use image.
func push(t *testing.T, dockerfile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	buildTag := repo + ":build-" + strconv.FormatInt(buildTagSeq.Add(1), 10)
	sh(t, "docker", "build", "-q", "-t", buildTag, dir)
	sh(t, "docker", "tag", buildTag, repo+":latest")
	sh(t, "docker", "push", "-q", repo+":latest")
	sh(t, "docker", "image", "rm", repo+":latest", buildTag)
	reg, err := registry.New(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := reg.Digest(context.Background(), repo+":latest")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func setup(t *testing.T) (*gate.Gate, string) {
	t.Helper()
	if exec.Command("docker", "inspect", "bosun-test-registry").Run() != nil {
		sh(t, "docker", "run", "-d", "--rm", "-p", "5055:5000", "--name", "bosun-test-registry", "registry:2")
	}
	for i := 0; ; i++ {
		resp, err := http.Get("http://localhost:5055/v2/")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i == 40 {
			t.Fatalf("registry did not start: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	name := "bosun-it-" + strings.ToLower(t.Name())
	t.Cleanup(func() {
		ids, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+name).Output()
		if f := strings.Fields(string(ids)); len(f) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, f...)...).Run()
		}
	})
	sockPath := os.Getenv("BOSUN_DOCKER_SOCK")
	if sockPath == "" {
		sockPath = "/var/run/docker.sock"
	}
	return &gate.Gate{D: docker.New(sockPath), Dir: t.TempDir(), Poll: 200 * time.Millisecond}, name
}

func runApp(t *testing.T, name string, extra ...string) {
	args := []string{"run", "-d", "--name", name, "--label", "bosun.enable=true", "--label", "bosun.health-timeout=3s"}
	args = append(append(args, extra...), repo+":latest")
	sh(t, "docker", args...)
}

func version(t *testing.T, name string) string {
	return sh(t, "docker", "exec", name, "cat", "/version")
}

func TestUpdateRevertSkipAndRollback(t *testing.T) {
	g, name := setup(t)
	ctx := context.Background()
	push(t, v1)
	runApp(t, name)

	d2 := push(t, v2)
	res, err := g.Update(ctx, name, d2, "")
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("update to v2: %+v %v", res, err)
	}
	if v := version(t, name); v != "v2" {
		t.Fatalf("running %s, want v2", v)
	}

	d3 := push(t, broken)
	res, err = g.Update(ctx, name, d3, "")
	if err != nil || res.Status != gate.StatusReverted {
		t.Fatalf("broken update: %+v %v", res, err)
	}
	if v := version(t, name); v != "v2" {
		t.Fatalf("after a failed update running %s, want v2", v)
	}
	if _, err := g.Update(ctx, name, d3, ""); err == nil || !strings.Contains(err.Error(), "skip list") {
		t.Fatalf("a skipped digest must be refused, got %v", err)
	}

	res, err = g.Rollback(ctx, name, false)
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("after rollback running %s, want v1", v)
	}
	if out := sh(t, "docker", "ps", "-a", "--filter", "name="+name+"-bosun-", "-q"); out != "" {
		t.Fatalf("old containers left behind: %s", out)
	}
}

// With bosun.logs=true, a rolled-back update brings back the dead
// container's output. Docker deletes that container seconds later, so this
// is the only chance to read it. The container has no TTY, so this also
// proves Docker's chunk headers are stripped.
func TestFailedUpdateLogsComeBack(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name, "--label", "bosun.logs=true")

	d := push(t, noisy)
	res, err := g.Update(context.Background(), name, d, "")
	if err != nil || res.Status != gate.StatusReverted {
		t.Fatalf("update: %+v %v", res, err)
	}
	// Exactly equal, not merely contained: without the header stripping the
	// value would be the same words behind Docker's chunk header.
	if res.Logs != "boom: no such table" {
		t.Fatalf("Logs = %q, want exactly the failed container's own words", res.Logs)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("running %s, want v1 back", v)
	}
}

func TestNewImageCmdIsUsed(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	d := push(t, newCmd)
	if res, err := g.Update(context.Background(), name, d, ""); err != nil || res.Status != gate.StatusDone {
		t.Fatalf("update: %+v %v", res, err)
	}
	if cmd := sh(t, "docker", "inspect", "-f", "{{json .Config.Cmd}}", name); cmd != `["sleep","7200"]` {
		t.Fatalf("Cmd = %s, want the new image's CMD", cmd)
	}
}

func TestVolumesAndNetworkSurvive(t *testing.T) {
	g, name := setup(t)
	suffix := strings.TrimPrefix(name, "bosun-it-")
	vol, net := "bosun-it-named-"+suffix, "bosun-it-net-"+suffix
	anon := ""
	t.Cleanup(func() {
		// Cleanups run last-first, so remove the container here before its volumes and network.
		exec.Command("docker", "rm", "-f", name).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
		if anon != "" {
			exec.Command("docker", "volume", "rm", anon).Run()
		}
		exec.Command("docker", "network", "rm", net).Run()
	})
	sh(t, "docker", "network", "create", net)
	push(t, v1)
	runApp(t, name, "--mount", "type=volume,dst=/anon", "-v", vol+":/named", "--network", net, "--network-alias", "web")
	anon = sh(t, "docker", "inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/anon"}}{{.Name}}{{end}}{{end}}`, name)
	sh(t, "docker", "exec", name, "sh", "-c", "echo a > /anon/f && echo n > /named/f")

	d := push(t, v2)
	if res, err := g.Update(context.Background(), name, d, ""); err != nil || res.Status != gate.StatusDone {
		t.Fatalf("update: %+v %v", res, err)
	}
	if v := version(t, name); v != "v2" {
		t.Fatalf("running %s, want v2", v)
	}
	if got := sh(t, "docker", "exec", name, "cat", "/anon/f", "/named/f"); got != "a\nn" {
		t.Fatalf("volume data after update = %q, want both files", got)
	}
	if nets := sh(t, "docker", "inspect", "-f", "{{json .NetworkSettings.Networks}}", name); !strings.Contains(nets, `"web"`) {
		t.Fatalf("network alias web lost: %s", nets)
	}
}

// crash leaves name as a stopped, renamed old container, as if the gate died
// mid-swap, and records it as pending.
func crash(t *testing.T, g *gate.Gate, name string) string {
	t.Helper()
	oldID := sh(t, "docker", "inspect", "-f", "{{.Id}}", name)
	sh(t, "docker", "stop", "-t", "1", name)
	sh(t, "docker", "rename", name, name+"-bosun-abc123")
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending = append(st.Pending, state.Pending{Name: name, OldID: oldID, TmpName: name + "-bosun-abc123"})
	if err := f.Save(st); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return oldID
}

func TestRecoverRestoresOld(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	crash(t, g, name)
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("running %s, want the old v1 back", v)
	}
	st, _ := state.Read(g.Dir)
	if len(st.Pending) != 0 || len(st.Events) != 1 || !strings.Contains(st.Events[0].Message, "old version is back") {
		t.Fatalf("state = %+v", st)
	}
}

var helperOnce sync.Once

// helperImage builds Bosun's image once, for the restore helper.
func helperImage(t *testing.T) string {
	t.Helper()
	const tag = "bosun-it-helper:latest"
	helperOnce.Do(func() { sh(t, "docker", "build", "-q", "-t", tag, "../..") })
	return tag
}

func TestBackupAndRestoreData(t *testing.T) {
	g, name := setup(t)
	ctx := context.Background()
	vol := name + "-named"
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "-v", name).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
	})
	// A host folder the gate writes and Docker can bind into the helper.
	dir, err := os.MkdirTemp("", "bosun-it-backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g.BackupDir, g.BackupSrc, g.HelperImage = dir, dir, helperImage(t)

	push(t, v1)
	runApp(t, name, "--label", "bosun.backup=true", "-v", vol+":/named", "--mount", "type=volume,dst=/anon")
	sh(t, "docker", "exec", name, "sh", "-c", "echo v1 > /named/f && echo v1 > /anon/f")

	d2 := push(t, v2)
	res, err := g.Update(ctx, name, d2, "")
	if err != nil || res.Status != gate.StatusDone || res.BackupBytes == 0 {
		t.Fatalf("update with backup: %+v %v", res, err)
	}
	m, err := backup.ReadManifest(filepath.Join(dir, name))
	if err != nil || len(m.Mounts) != 2 {
		t.Fatalf("manifest = %+v %v, want 2 mounts", m, err)
	}

	sh(t, "docker", "exec", name, "sh", "-c", "echo v2 > /named/f && echo later > /named/new.txt && echo v2 > /anon/f")
	res, err = g.Rollback(ctx, name, true)
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("rollback --with-data: %+v %v", res, err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("running %s, want v1", v)
	}
	for file, want := range map[string]string{"/named/f": "v1", "/anon/f": "v1"} {
		if got := sh(t, "docker", "exec", name, "cat", file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if exec.Command("docker", "exec", name, "test", "-e", "/named/new.txt").Run() == nil {
		t.Error("a file made after the backup survived the restore")
	}
	if out := sh(t, "docker", "ps", "-a", "--filter", "name="+name+"-bosun-restore", "-q"); out != "" {
		t.Errorf("restore helper left behind: %s", out)
	}
}

func TestRecoverKeepsHealthyNew(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	oldID := crash(t, g, name)
	runApp(t, name) // the "new" container came up before the crash
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exec.Command("docker", "inspect", oldID).Run() == nil {
		t.Fatal("old container must be removed")
	}
	st, _ := state.Read(g.Dir)
	if len(st.Events) != 1 || !strings.Contains(st.Events[0].Message, "new version is running") {
		t.Fatalf("events = %+v", st.Events)
	}
}
