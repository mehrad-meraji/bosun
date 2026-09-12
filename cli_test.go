package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestCheckSettings(t *testing.T) {
	s, b, r := stateDir, backupDir, runDir
	t.Cleanup(func() { stateDir, backupDir, runDir = s, b, r })
	for _, tc := range []struct {
		state, backup, run string
		ok                 bool
	}{
		{"/var/lib/bosun", "/var/lib/bosun-backups", "/run/bosun", true},
		{"/var/lib/bosun", "/var/lib/bosun", "/run/bosun", false},                         // same folder
		{"/var/lib/b/state", "/var/lib/b", "/run/bosun", false},                           // state inside backup
		{"/var/lib/b", "/var/lib/b/backups", "/run/bosun", false},                         // backup inside state
		{"/var/lib/bosun", "/var/lib/bosun-backups", "/var/lib/bosun/run", false},         // run inside state
		{"/var/lib/bosun", "/var/lib/bosun-backups", "/var/lib/bosun-backups/run", false}, // run inside backup
		{"/run/bosun/state", "/var/lib/bosun-backups", "/run/bosun", false},               // state inside run
	} {
		stateDir, backupDir, runDir = tc.state, tc.backup, tc.run
		if err := checkSettings(); (err == nil) != tc.ok {
			t.Errorf("checkSettings(%+v) = %v, want ok=%v", tc, err, tc.ok)
		}
	}
}

func TestCheckSettingsControlLink(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	save := func() func() {
		u, to, p, i := controlURL, controlToken, controlPoll, controlInsecure
		return func() { controlURL, controlToken, controlPoll, controlInsecure = u, to, p, i }
	}
	defer save()()

	controlURL, controlToken, controlPoll, controlInsecure = "https://server", "/srv/token", "60s", false
	if err := checkSettings(); err == nil || !strings.Contains(err.Error(), "/etc/bosun") {
		t.Errorf("a token outside /etc/bosun must be refused: %v", err)
	}

	// With the link off, none of it is checked, whatever the other values say.
	controlURL, controlToken = "", tok
	if err := checkSettings(); err != nil {
		t.Errorf("with the link off, nothing is checked: %v", err)
	}
}

func TestPollEvery(t *testing.T) {
	save := controlPoll
	defer func() { controlPoll = save }()
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{{"60s", false}, {"15s", false}, {"14s", true}, {"", true}, {"soon", true}} {
		controlPoll = tc.in
		if _, err := pollEvery(); (err != nil) != tc.wantErr {
			t.Errorf("pollEvery(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestBackupCol(t *testing.T) {
	dir := t.TempDir()
	b := backupDir
	t.Cleanup(func() { backupDir = b })
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
