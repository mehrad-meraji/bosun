package gate

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
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
		{Type: "volume", Name: "bosun-state", Destination: "/var/lib/bosun"},
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
	for _, bad := range []string{"/srv/data", "SECRET=x", "PATH=/usr/bin", "bosun-state", "/var/lib/bosun"} {
		if strings.Contains(s, bad) {
			t.Errorf("updater body must not carry %s", bad)
		}
	}
}

func TestRecoverWhenOldContainerIsGone(t *testing.T) {
	// Track which requests were made
	var startCalled bool
	var deleteCalled bool

	fakeDocker := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			if strings.Contains(r.URL.Path, "/containers/old-id-prefix") {
				// Old container not found
				w.WriteHeader(404)
				io.WriteString(w, `{"message":"No such container"}`)
				return
			}
			if strings.Contains(r.URL.Path, "/containers/app") && !strings.Contains(r.URL.Path, "/start") {
				// Current container at name exists but is stopped
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"Id":"new-id-123","Name":"/app","State":{"Running":false,"Status":"exited"},"Mounts":[],"Config":{"Image":"app:v1"}}`)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such container"}`)
		case "POST":
			if strings.Contains(r.URL.Path, "/start") {
				startCalled = true
				w.WriteHeader(204)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such endpoint"}`)
		case "DELETE":
			deleteCalled = true
			w.WriteHeader(204)
		default:
			w.WriteHeader(400)
			io.WriteString(w, `{"message":"unexpected method"}`)
		}
	})

	// Create temp dir for state
	stateDir, err := os.MkdirTemp("", "gate-state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })

	// Write pending record
	f, st, err := state.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending = []state.Pending{
		{Name: "app", OldID: "old-id-prefix", TmpName: "app-bosun-tmp"},
	}
	if err := f.Save(st); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Create gate with fake Docker and state dir
	g := &Gate{D: fakeDocker, Dir: stateDir}

	// Call Recover
	ctx := context.Background()
	err = g.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	// Verify that start was called (to restart the existing container at name)
	if !startCalled {
		t.Error("expected POST .../start for the current container")
	}

	// Verify that no DELETE request was made (to remove containers)
	if deleteCalled {
		t.Error("no DELETE request should be made when old is gone and new exists")
	}

	// Verify that pending list is now empty
	f, st, err = state.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if len(st.Pending) != 0 {
		t.Errorf("expected Pending list to be empty, got %d records", len(st.Pending))
	}

	// Verify that an event was added
	if len(st.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(st.Events))
		return
	}
	if st.Events[0].Kind != "recovered" || st.Events[0].Name != "app" {
		t.Errorf("unexpected event: %v", st.Events[0])
	}
	if !strings.Contains(st.Events[0].Message, "old container was already gone") {
		t.Errorf("event message should mention old container was gone: %s", st.Events[0].Message)
	}
}

func fake(t *testing.T, h http.HandlerFunc) *docker.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "bd")
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
	return docker.New(p)
}
