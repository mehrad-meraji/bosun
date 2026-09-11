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
