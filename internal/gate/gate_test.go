package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func ctr(running bool, restarts int, health string) *docker.Container {
	c := &docker.Container{RestartCount: restarts}
	c.State.Running = running
	c.State.Status = "running"
	if health != "" {
		c.State.Health = &docker.Health{Status: health}
	}
	return c
}

func TestVerdict(t *testing.T) {
	const timeout = 10 * time.Second
	for _, tc := range []struct {
		name    string
		c       *docker.Container
		elapsed time.Duration
		done    bool
		ok      bool
	}{
		{"no healthcheck, still waiting", ctr(true, 0, ""), time.Second, false, false},
		{"no healthcheck, stable until timeout", ctr(true, 0, ""), timeout, true, true},
		{"exited", ctr(false, 0, ""), time.Second, true, false},
		{"restarted once", ctr(true, 1, ""), time.Second, true, false},
		{"healthcheck starting", ctr(true, 0, "starting"), time.Second, false, false},
		{"healthy", ctr(true, 0, "healthy"), time.Second, true, true},
		{"unhealthy", ctr(true, 0, "unhealthy"), time.Second, true, false},
		{"never healthy", ctr(true, 0, "starting"), timeout, true, false},
	} {
		done, err := verdict(tc.c, tc.elapsed, timeout)
		if done != tc.done || (done && (err == nil) != tc.ok) {
			t.Errorf("%s: done=%v err=%v; want done=%v ok=%v", tc.name, done, err, tc.done, tc.ok)
		}
	}
}

func TestTrackable(t *testing.T) {
	for ref, want := range map[string]bool{
		"nginx:1.27":            true,
		"ghcr.io/me/app":        true,
		"sha256:abcdef":         false,
		"nginx@sha256:abcdef":   false,
		"nginx:1.27@sha256:abc": false,
		"":                      false,
	} {
		c := &docker.Container{}
		c.Config.Image = ref
		if _, ok := trackable(c); ok != want {
			t.Errorf("trackable(%q) = %v, want %v", ref, ok, want)
		}
	}
}

func TestDigestsOf(t *testing.T) {
	img := &docker.Image{RepoDigests: []string{"nginx@sha256:aa", "localhost:5000/nginx@sha256:bb"}}
	got := digestsOf(img)
	if len(got) != 2 || got[0] != "sha256:aa" || got[1] != "sha256:bb" {
		t.Errorf("digestsOf = %v", got)
	}
}

func TestTimeoutOf(t *testing.T) {
	c := &docker.Container{}
	if timeoutOf(c) != 60*time.Second {
		t.Error("default must be 60s")
	}
	c.Config.Labels = map[string]string{LabelTimeout: "2m"}
	if timeoutOf(c) != 2*time.Minute {
		t.Error("label must win")
	}
	c.Config.Labels[LabelTimeout] = "soon"
	if timeoutOf(c) != 60*time.Second {
		t.Error("bad label must fall back to 60s")
	}
}

func TestWatchedBadName(t *testing.T) {
	// Test that bad names are refused without touching Docker (D is nil)
	g := &Gate{D: nil, Dir: ""}
	for _, badName := range []string{
		"",                       // empty
		"_web",                   // starts with underscore
		"-web",                   // starts with dash
		".web",                   // starts with dot
		"web/name",               // contains slash
		"web?name",               // contains question mark
		strings.Repeat("a", 129), // too long
	} {
		_, _, err := g.watched(context.Background(), badName)
		if err == nil || !isRefused(err) {
			t.Errorf("watched(%q) should refuse, got %v", badName, err)
		}
	}
}

func TestWatchedNameIDMismatch(t *testing.T) {
	// Test that using a container ID when a name is required is refused
	g := fakeGate(t, func(w http.ResponseWriter, r *http.Request) {
		// The request might be for abc123 (ID prefix used as name)
		if r.URL.Path == "/v1.44/containers/abc123/json" {
			// Return a container with a different name and required labels
			resp := docker.Container{
				ID:   "abc123def456",
				Name: "/web",
				Config: docker.ContainerConfig{
					Image:  "nginx:1.27",
					Labels: map[string]string{LabelEnable: "true"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	// Using the ID prefix should be refused (doesn't match the actual name "web")
	_, _, err := g.watched(context.Background(), "abc123")
	if err == nil || !isRefused(err) {
		t.Errorf("watched(id prefix) should refuse, got %v", err)
	}
	// Verify the error is specifically about the name/ID mismatch, not the label
	if !strings.Contains(err.Error(), "not a container name") {
		t.Errorf("expected 'not a container name' in error, got %v", err)
	}
}

// fakeGate creates a Gate with a fake Docker server for testing.
func fakeGate(t *testing.T, h http.HandlerFunc) *Gate {
	t.Helper()
	dir, err := os.MkdirTemp("", "bg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return &Gate{D: docker.New(p), Dir: dir}
}

func isRefused(err error) bool {
	_, ok := err.(*RefusedError)
	return ok
}

// dockerFake is a small Docker for swap tests. Containers and images are
// inspect JSON keyed by name or ID; anything missing is a 404. fail lists
// "METHOD path" calls that return 500. Every call is kept in calls as
// "METHOD path?query", with the query unescaped.
type dockerFake struct {
	mu         sync.Mutex
	list       string
	containers map[string]string
	images     map[string]string
	fail       map[string]bool
	calls      []string
	onStop     func()
}

const (
	oldCtr  = `{"Id":"old-id","Name":"/app","Image":"sha256:old","State":{"Running":true},"Config":{"Image":"app:v1","Labels":{"bosun.enable":"true","bosun.health-timeout":"1ms"}},"HostConfig":{},"Mounts":[]}`
	oldImg  = `{"Id":"sha256:old","RepoDigests":["app@sha256:d1"],"Config":{}}`
	pullImg = `{"Id":"sha256:new","RepoDigests":["app@sha256:d2"]}`
)

func newCtr(running bool, health string) string {
	h := "null"
	if health != "" {
		h = fmt.Sprintf(`{"Status":%q}`, health)
	}
	return fmt.Sprintf(`{"Id":"new-id","Name":"/app","State":{"Running":%v,"Status":"exited","Health":%s},"Config":{"Labels":{"bosun.health-timeout":"1ms"}}}`, running, h)
}

// swapFake is the usual case: app runs sha256:old, the pull gives sha256:new,
// and the new container runs (or not).
func swapFake(newRunning bool) *dockerFake {
	return &dockerFake{
		containers: map[string]string{"app": oldCtr, "old-id": oldCtr, "new-id": newCtr(newRunning, "")},
		images:     map[string]string{"app:v1": pullImg, "sha256:old": oldImg},
		fail:       map[string]bool{},
	}
}

func (f *dockerFake) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+docker.APIVersion)
	call := r.Method + " " + path
	q, _ := url.QueryUnescape(r.URL.RawQuery)
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSuffix(call+"?"+q, "?"))
	fail := f.fail[call]
	f.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"boom"}`)
		return
	}
	if strings.HasSuffix(path, "/stop") && f.onStop != nil {
		f.onStop()
	}
	body, found := "", true
	switch {
	case call == "GET /containers/json":
		body = f.list
	case r.Method == "GET" && strings.HasPrefix(path, "/containers/"):
		body, found = f.containers[strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")]
	case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
		body, found = f.images[strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")]
	case call == "POST /containers/create":
		body = `{"Id":"new-id"}`
	case call == "POST /images/create":
		body = `{}`
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such object"}`)
		return
	}
	io.WriteString(w, body)
}

func (f *dockerFake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, call)
}

// calledAfter reports whether call happens after the first time first does.
func (f *dockerFake) calledAfter(first, call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.calls, first)
	return i >= 0 && slices.Contains(f.calls[i+1:], call)
}

func (f *dockerFake) gate(t *testing.T) *Gate {
	g := fakeGate(t, f.serve)
	g.Poll = time.Millisecond
	return g
}

// setEntry writes a state entry and pending swaps before a test.
func setEntry(t *testing.T, g *Gate, name string, e state.Entry, pending ...state.Pending) {
	t.Helper()
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	*st.Entry(name) = e
	st.Pending = pending
	if err := f.Save(st); err != nil {
		t.Fatal(err)
	}
}

const retagOld = "POST /images/sha256:old/tag?repo=app&tag=v1"

func TestUpdateRevertedPutsTagBackAndSkips(t *testing.T) {
	f := swapFake(false)
	g := f.gate(t)
	f.onStop = func() {
		st, err := state.Read(g.Dir)
		if err != nil || len(st.Pending) != 1 || st.Pending[0].Digest != "sha256:d2" {
			t.Errorf("pending record during the swap = %+v %v, want digest sha256:d2", st.Pending, err)
		}
	}
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil || res.Status != StatusReverted {
		t.Fatalf("Update = %+v %v, want reverted", res, err)
	}
	if !strings.Contains(res.Message, "`bosun skip clear app` allows it again") {
		t.Errorf("message lacks the next step: %s", res.Message)
	}
	if !f.called(retagOld) {
		t.Errorf("tag not put back on the old image; calls: %v", f.calls)
	}
	st, _ := state.Read(g.Dir)
	if !slices.Contains(st.Entry("app").Skip, "sha256:d2") || len(st.Pending) != 0 {
		t.Errorf("state = %+v, want d2 skipped and nothing pending", st)
	}
}

func TestUpdateSameImageIsANoop(t *testing.T) {
	f := swapFake(true)
	f.images["app:v1"] = `{"Id":"sha256:old","RepoDigests":["app@sha256:d2"]}`
	g := f.gate(t)
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil || res.Status != StatusDone || !strings.Contains(res.Message, "already running app:v1") {
		t.Fatalf("Update = %+v %v, want done, already running", res, err)
	}
	if f.called("POST /containers/old-id/stop") {
		t.Error("the container was swapped onto the image it already runs")
	}
	if st, _ := state.Read(g.Dir); st.Entry("app").Prev != "" {
		t.Errorf("nothing should be kept for rollback: %+v", st.Entry("app"))
	}
}

func TestUpdateRefusesStoppedContainer(t *testing.T) {
	f := swapFake(true)
	f.containers["app"] = strings.Replace(oldCtr, `"Running":true`, `"Running":false`, 1)
	_, err := f.gate(t).Update(context.Background(), "app", "sha256:d2", "")
	if !isRefused(err) || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if f.called("POST /images/create") {
		t.Error("pulled for a stopped container")
	}
}

func TestRollbackPutsTagBackUnlessItWorks(t *testing.T) {
	const retagPrev = "POST /images/bosun/prev/app:abc/tag?repo=app&tag=v1"
	for _, tc := range []struct {
		name       string
		newRunning bool
		stopFails  bool
		restore    bool
	}{
		{"stop fails", true, true, true},
		{"old version not healthy", false, false, true},
		{"works", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := swapFake(tc.newRunning)
			f.fail["POST /containers/old-id/stop"] = tc.stopFails
			g := f.gate(t)
			setEntry(t, g, "app", state.Entry{Prev: "bosun/prev/app:abc"})
			_, _ = g.Rollback(context.Background(), "app", false)
			if !f.called(retagPrev) {
				t.Fatalf("tag not moved to the kept image; calls: %v", f.calls)
			}
			if got := f.calledAfter(retagPrev, retagOld); got != tc.restore {
				t.Errorf("tag put back = %v, want %v; calls: %v", got, tc.restore, f.calls)
			}
		})
	}
}

func TestSwapRenameFailSaysHowToStart(t *testing.T) {
	f := swapFake(true)
	f.fail["POST /containers/old-id/rename"] = true
	f.fail["POST /containers/old-id/start"] = true
	_, err := f.gate(t).Update(context.Background(), "app", "sha256:d2", "")
	if err == nil || !strings.Contains(err.Error(), "app is stopped") || !strings.Contains(err.Error(), "docker start app") {
		t.Fatalf("error must say app is stopped and how to start it, got %v", err)
	}
}

func TestSwapRevertStartFailSaysHowToStart(t *testing.T) {
	f := swapFake(false)
	f.fail["POST /containers/old-id/start"] = true
	g := f.gate(t)
	_, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err == nil || !strings.Contains(err.Error(), "app is stopped; start it with: docker start app") || strings.Contains(err.Error(), "stopped as") {
		t.Fatalf("error must say app is stopped under its own name, got %v", err)
	}
	if st, _ := state.Read(g.Dir); len(st.Pending) != 1 {
		t.Errorf("pending record must stay for crash recovery: %+v", st.Pending)
	}
}

func TestListSkipsVanishedContainer(t *testing.T) {
	f := swapFake(true)
	f.list = `[{"Id":"gone"},{"Id":"old-id"}]`
	ws, err := f.gate(t).List(context.Background())
	if err != nil || len(ws) != 1 || ws[0].Name != "app" {
		t.Fatalf("List = %+v %v, want only app", ws, err)
	}
}
