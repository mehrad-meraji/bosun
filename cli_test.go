package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/gate"
)

func TestSplitArgs(t *testing.T) {
	pos, flags := splitArgs([]string{"nginx", "--yes", "--dry-run"})
	if !reflect.DeepEqual(pos, []string{"nginx"}) || !flags["yes"] || !flags["dry-run"] {
		t.Fatalf("got %v %v", pos, flags)
	}
	if err := checkFlags(flags, "yes"); err == nil {
		t.Fatal("want an error for --dry-run when only --yes is allowed")
	}
}

func TestAgo(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{72 * time.Hour, "3 days ago"},
	} {
		if got := ago(time.Now().Add(-tc.d)); got != tc.want {
			t.Errorf("ago(-%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if ago(time.Time{}) != "never" {
		t.Error("zero time must be never")
	}
}

func TestInside(t *testing.T) {
	for _, tc := range []struct {
		child, parent string
		want          bool
	}{
		{"/run/bosun", "/run/bosun", true},
		{"/run/bosun/state", "/run/bosun", true},
		{"/run/bosun-backups", "/run/bosun", false},
		{"/var/lib/bosun-backups", "/etc/bosun", false},
		{"/etc/bosun/../bosun/x", "/etc/bosun", true},
	} {
		if got := inside(tc.child, tc.parent); got != tc.want {
			t.Errorf("inside(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}

func TestBackupCol(t *testing.T) {
	dir := t.TempDir()
	backupDir = dir
	if got := backupCol(gate.Watched{Name: "web"}); got != "off" {
		t.Errorf("no label: %q", got)
	}
	if got := backupCol(gate.Watched{Name: "web", Backup: true}); got != "on, none yet" {
		t.Errorf("no backup yet: %q", got)
	}
	os.MkdirAll(filepath.Join(dir, "web"), 0o700)
	backup.WriteManifest(filepath.Join(dir, "web"), &backup.Manifest{Mounts: []backup.Mount{{Bytes: 3 << 30}}})
	if got := backupCol(gate.Watched{Name: "web", Backup: true}); got != "on, 3.0 GB" {
		t.Errorf("with backup: %q", got)
	}
}
