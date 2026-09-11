package gate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/docker"
)

func TestUpdaterBodyIsLockedDown(t *testing.T) {
	self := &docker.Container{Image: "sha256:img"}
	self.Config.Env = []string{"BOSUN_SCHEDULE=0 5 * * *", "PATH=/usr/bin", "SECRET=x"}
	self.Mounts = []docker.Mount{
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		{Type: "volume", Name: "bosun-run", Destination: "/run/bosun"},
		{Type: "bind", Source: "/srv/bosun/notify.txt", Destination: "/etc/bosun/notify.txt"},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/etc/bosun/sneaky"},
		{Type: "bind", Source: "/srv/data", Destination: "/data"},
	}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun"))
	s := string(b)

	if strings.Contains(s, "docker.sock") {
		t.Errorf("updater must never get docker.sock: %s", s)
	}
	for _, want := range []string{
		`"Image":"sha256:img"`,
		`"bosun-run:/run/bosun"`,
		`"/srv/bosun/notify.txt:/etc/bosun/notify.txt:ro"`,
		`"CapDrop":["ALL"]`,
		`"ReadonlyRootfs":true`,
		`"no-new-privileges:true"`,
		`"BOSUN_SCHEDULE=0 5 * * *"`,
		`"bosun.managed-by":"bosun-gate"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("updater body lacks %s:\n%s", want, s)
		}
	}
	for _, bad := range []string{"/srv/data", "SECRET=x", "PATH=/usr/bin"} {
		if strings.Contains(s, bad) {
			t.Errorf("updater body must not carry %s", bad)
		}
	}
}
