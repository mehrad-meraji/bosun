package gate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func TestRestoreBodyIsLockedDown(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{
		{Type: "volume", Name: "appdata", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/app/conf", Destination: "/conf", RW: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
	}}
	m := &backup.Manifest{Mounts: []backup.Mount{{Dest: "/data", File: "0.tar"}, {Dest: "/conf", File: "1.tar"}}}
	body, err := restoreBody("sha256:bosun", "bosun-backups", "app", m, c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	s := string(b)
	for _, want := range []string{
		`"Image":"sha256:bosun"`,
		`"User":"0:0"`,
		`"NetworkMode":"none"`,
		`"ReadonlyRootfs":true`,
		`"CapDrop":["ALL"]`,
		`"CapAdd":["CHOWN","DAC_OVERRIDE","FOWNER"]`,
		`"no-new-privileges:true"`,
		`"bosun-backups:/backup:ro"`,
		`"appdata:/data/0"`,
		`"/srv/app/conf:/data/1"`,
		`"/backup/app/0.tar=/data/0"`,
		`"/backup/app/1.tar=/data/1"`,
		`"bosun.managed-by":"bosun-gate"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("restore body lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "docker.sock") {
		t.Errorf("restore helper must never get docker.sock: %s", s)
	}
}

func TestRestoreBodyRefusesBadInput(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{{Type: "volume", Name: "appdata", Destination: "/data", RW: true}}}
	for _, m := range []*backup.Manifest{
		{Mounts: []backup.Mount{{Dest: "/gone", File: "0.tar"}}},       // mount no longer there
		{Mounts: []backup.Mount{{Dest: "/data", File: "../x.tar"}}},    // odd file name
		{Mounts: []backup.Mount{{Dest: "/data", File: "0.tar:/etc"}}}, // bind injection
	} {
		if _, err := restoreBody("img", "src", "app", m, c); err == nil {
			t.Errorf("restoreBody(%+v) must fail", m.Mounts)
		}
	}
}

func TestDataBackupMustMatchTheKeptVersion(t *testing.T) {
	f := swapFake(true)
	f.images["bosun/prev/app:abc"] = `{"Id":"sha256:prev"}`
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	if _, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err == nil || !strings.Contains(err.Error(), "bosun.backup=true") {
		t.Fatalf("no backup: want a hint about the label, got %v", err)
	}
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:other"})
	if _, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("wrong version: got %v", err)
	}
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:prev"})
	if m, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err != nil || m.Image != "sha256:prev" {
		t.Fatalf("matching backup: %+v %v", m, err)
	}
}

// rollbackWithDataFake sets up a fake app+backup ready for a --with-data
// rollback: a manifest matching the kept image bosun/prev/app:abc, the gate's
// own helper image/backup source set directly, and Prev recorded in state.
func rollbackWithDataFake(t *testing.T, newRunning bool) (*dockerFake, *Gate) {
	t.Helper()
	f := swapFake(newRunning)
	f.containers["app"] = backupCtr
	f.containers["old-id"] = backupCtr
	f.images["bosun/prev/app:abc"] = `{"Id":"sha256:prev"}`
	f.replies = map[string]string{"GET /containers/new-id/logs": "boom"}
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	g.HelperImage = "img"
	g.BackupSrc = "src"
	dir := filepath.Join(g.BackupDir, "app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:prev", Mounts: []backup.Mount{{Dest: "/data", File: "0.tar"}}}); err != nil {
		t.Fatal(err)
	}
	setEntry(t, g, "app", state.Entry{Prev: "bosun/prev/app:abc"})
	return f, g
}

func TestRollbackWithData(t *testing.T) {
	t.Run("helper refuses (exit 2): old container restarted, nothing created", func(t *testing.T) {
		f, g := rollbackWithDataFake(t, true)
		f.replies["POST /containers/new-id/wait"] = `{"StatusCode":2}`
		_, err := g.Rollback(context.Background(), "app", true)
		if err == nil || !strings.Contains(err.Error(), "current version is running again") {
			t.Fatalf("want the current version running again, got %v", err)
		}
		if !f.calledAfter("POST /containers/new-id/wait", "POST /containers/old-id/start") {
			t.Errorf("old container not restarted after the helper refused; calls: %v", f.calls)
		}
		if f.called("POST /containers/create?name=app") {
			t.Errorf("must not have created the new app container; calls: %v", f.calls)
		}
	})

	t.Run("helper fails (exit 1): incomplete data, nothing restarted", func(t *testing.T) {
		f, g := rollbackWithDataFake(t, true)
		f.replies["POST /containers/new-id/wait"] = `{"StatusCode":1}`
		_, err := g.Rollback(context.Background(), "app", true)
		if err == nil || !strings.Contains(err.Error(), "incomplete data") || !strings.Contains(err.Error(), "--with-data` again") {
			t.Fatalf("want an incomplete-data message telling the user to retry, got %v", err)
		}
		if f.calledAfter("POST /containers/new-id/wait", "POST /containers/old-id/start") {
			t.Errorf("must not restart with incomplete data; calls: %v", f.calls)
		}
	})

	t.Run("helper ok, new version unhealthy: reverted, stays stopped", func(t *testing.T) {
		f, g := rollbackWithDataFake(t, false)
		f.replies["POST /containers/new-id/wait"] = `{"StatusCode":0}`
		res, err := g.Rollback(context.Background(), "app", true)
		if err != nil || res.Status != StatusReverted || !strings.Contains(res.Message, "is stopped") {
			t.Fatalf("Rollback = %+v %v, want reverted and stopped", res, err)
		}
		if f.calledAfter("POST /containers/new-id/wait", "POST /containers/old-id/start") {
			t.Errorf("must never restart the old container after a with-data revert; calls: %v", f.calls)
		}
	})

	t.Run("helper ok, rename fails: stopped with the backup's data", func(t *testing.T) {
		f, g := rollbackWithDataFake(t, true)
		f.replies["POST /containers/new-id/wait"] = `{"StatusCode":0}`
		f.fail["POST /containers/old-id/rename"] = true
		_, err := g.Rollback(context.Background(), "app", true)
		if err == nil || !strings.Contains(err.Error(), "stopped with the data from the backup") {
			t.Fatalf("want a stopped-with-backup-data message, got %v", err)
		}
		if f.calledAfter("POST /containers/new-id/wait", "POST /containers/old-id/start") {
			t.Errorf("must not start after the rename failed; calls: %v", f.calls)
		}
	})

	t.Run("helper ok, healthy: done", func(t *testing.T) {
		f, g := rollbackWithDataFake(t, true)
		f.replies["POST /containers/new-id/wait"] = `{"StatusCode":0}`
		res, err := g.Rollback(context.Background(), "app", true)
		if err != nil || res.Status != StatusDone || !strings.Contains(res.Message, "Data is from the backup") {
			t.Fatalf("Rollback = %+v %v, want done with the data note; calls: %v", res, err, f.calls)
		}
	})
}

func TestRollbackWithDataRefusesBeforeChangingAnything(t *testing.T) {
	f := swapFake(true)
	f.images["bosun/prev/app:abc"] = `{"Id":"sha256:prev"}`
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	setEntry(t, g, "app", state.Entry{Prev: "bosun/prev/app:abc"})
	if _, err := g.Rollback(context.Background(), "app", true); err == nil {
		t.Fatal("want an error: there is no data backup")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST") {
			t.Fatalf("changed something before refusing: %v", f.calls)
		}
	}
}
