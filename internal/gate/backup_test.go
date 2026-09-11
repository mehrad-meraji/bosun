package gate

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// backupCtr is oldCtr with backups on and one writable volume at /data.
var backupCtr = strings.Replace(
	strings.Replace(oldCtr, `"Mounts":[]`, `"Mounts":[{"Type":"volume","Name":"appdata","Destination":"/data","RW":true}]`, 1),
	`"bosun.enable":"true"`, `"bosun.enable":"true","bosun.backup":"true"`, 1)

func dataTar(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "data/f.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2})
	tw.Write([]byte("hi"))
	tw.Close()
	return buf.String()
}

func backupFake(t *testing.T, newRunning bool) (*dockerFake, *Gate) {
	f := swapFake(newRunning)
	f.containers["app"] = backupCtr
	f.containers["old-id"] = backupCtr
	f.containers["old-id/archive"] = dataTar(t)
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	return f, g
}

func TestBackupMounts(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{
		{Type: "volume", Name: "v", Destination: "/v", RW: true},
		{Type: "bind", Source: "/srv/x", Destination: "/x", RW: true},
		{Type: "volume", Name: "ro", Destination: "/ro", RW: false},
		{Type: "tmpfs", Destination: "/tmp", RW: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
	}}
	got := backupMounts(c)
	if len(got) != 2 || got[0].Destination != "/v" || got[1].Destination != "/x" {
		t.Fatalf("backupMounts = %+v, want /v and /x only", got)
	}
}

func TestTakeBackupReplacesTheOldOne(t *testing.T) {
	f, g := backupFake(t, true)
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "junk"), []byte("old"), 0o600)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:older"})

	c, err := g.D.Inspect(context.Background(), "old-id")
	if err != nil {
		t.Fatal(err)
	}
	m, err := g.takeBackup(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if m.Image != "sha256:old" || len(m.Mounts) != 1 || m.Mounts[0].Dest != "/data" || m.Mounts[0].Bytes != int64(len(f.containers["old-id/archive"])) {
		t.Fatalf("manifest = %+v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "0.tar")); string(b) != f.containers["old-id/archive"] {
		t.Error("0.tar does not hold the archive")
	}
	if _, err := os.Stat(filepath.Join(dir, "junk")); err == nil {
		t.Error("the old backup was not replaced")
	}
	for _, left := range []string{dir + ".new", dir + ".old"} {
		if _, err := os.Stat(left); err == nil {
			t.Errorf("%s left behind", left)
		}
	}
}

func TestTakeBackupFailureKeepsTheOldOne(t *testing.T) {
	f, g := backupFake(t, true)
	f.fail["GET /containers/old-id/archive"] = true
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:older"})
	c, _ := g.D.Inspect(context.Background(), "old-id")
	if _, err := g.takeBackup(context.Background(), c); err == nil {
		t.Fatal("want an error")
	}
	if m, err := backup.ReadManifest(dir); err != nil || m.Image != "sha256:older" {
		t.Errorf("old backup changed: %+v %v", m, err)
	}
	if _, err := os.Stat(dir + ".new"); err == nil {
		t.Error("partial copy left behind")
	}
}

func TestTakeBackupLeavesOtherBackupsAlone(t *testing.T) {
	_, g := backupFake(t, true)
	newDir := filepath.Join(g.BackupDir, "app.new")
	oldDir := filepath.Join(g.BackupDir, "app.old")
	os.MkdirAll(newDir, 0o700)
	os.MkdirAll(oldDir, 0o700)
	backup.WriteManifest(newDir, &backup.Manifest{Image: "sha256:newcontainer"})
	backup.WriteManifest(oldDir, &backup.Manifest{Image: "sha256:oldcontainer"})

	c, err := g.D.Inspect(context.Background(), "old-id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.takeBackup(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if m, err := backup.ReadManifest(newDir); err != nil || m.Image != "sha256:newcontainer" {
		t.Errorf("app.new's backup was disturbed: %+v %v", m, err)
	}
	if m, err := backup.ReadManifest(oldDir); err != nil || m.Image != "sha256:oldcontainer" {
		t.Errorf("app.old's backup was disturbed: %+v %v", m, err)
	}
	for _, left := range []string{
		filepath.Join(g.BackupDir, ".app.new"),
		filepath.Join(g.BackupDir, ".app.old"),
	} {
		if _, err := os.Stat(left); err == nil {
			t.Errorf("%s left behind", left)
		}
	}
}

func TestUpdateBacksUpWhileStopped(t *testing.T) {
	f, g := backupFake(t, true)
	g.WarnSize = 1 // any backup is "big", to test the one-time warning
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil || res.Status != StatusDone {
		t.Fatalf("Update = %+v %v", res, err)
	}
	if !f.calledAfter("POST /containers/old-id/stop", "GET /containers/old-id/archive?path=/data") {
		t.Errorf("backup must happen after the stop; calls: %v", f.calls)
	}
	if !f.calledAfter("GET /containers/old-id/archive?path=/data", "POST /containers/create?name=app") {
		t.Errorf("backup must happen before the new container; calls: %v", f.calls)
	}
	if res.BackupBytes == 0 || !strings.Contains(res.Message, "backup") || !strings.Contains(res.Message, "long downtime") {
		t.Errorf("result = %+v, want backup size, time and a warning", res)
	}
	if st, _ := state.Read(g.Dir); !st.Entry("app").WarnedBig {
		t.Error("warning must be remembered")
	}
	// Second time: no repeat of the warning.
	f.containers["app"] = backupCtr
	res, _ = g.Update(context.Background(), "app", "sha256:d2", "")
	if strings.Contains(res.Message, "long downtime") {
		t.Errorf("warning repeated: %s", res.Message)
	}
}

func TestUpdateBackupFailureStopsTheUpdate(t *testing.T) {
	f, g := backupFake(t, true)
	f.fail["GET /containers/old-id/archive"] = true
	_, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err == nil || !strings.Contains(err.Error(), "backup failed") || !strings.Contains(err.Error(), "running again") {
		t.Fatalf("want a backup failure that says the old version runs, got %v", err)
	}
	if f.called("POST /containers/create?name=app") {
		t.Error("updated without a backup")
	}
	if !f.calledAfter("GET /containers/old-id/archive?path=/data", "POST /containers/old-id/start") {
		t.Errorf("old container not started again; calls: %v", f.calls)
	}
	if st, _ := state.Read(g.Dir); len(st.Pending) != 0 {
		t.Errorf("pending record left: %+v", st.Pending)
	}
}

func TestNoBackupWithoutLabel(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	if _, err := g.Update(context.Background(), "app", "sha256:d2", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/archive") {
			t.Fatalf("backed up without bosun.backup=true: %v", f.calls)
		}
	}
}
