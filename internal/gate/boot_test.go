package gate

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
		{Type: "volume", Name: "bosun-backups", Destination: "/var/lib/bosun-backups"},
	}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun"))
	s := string(b)

	if strings.Contains(s, "docker.sock") {
		t.Errorf("updater must never get docker.sock: %s", s)
	}
	for _, want := range []string{
		`"Image":"sha256:img"`,
		`"User":"65532:65532"`,
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
	for _, bad := range []string{"/srv/data", "SECRET=x", "PATH=/usr/bin", "bosun-state", "/var/lib/bosun", "bosun-backups", "/var/lib/bosun-backups"} {
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

func TestRecoverWhenOldContainerIsGoneButStartFails(t *testing.T) {
	// Track which requests were made
	var startCalled bool

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
				// Start fails with server error
				w.WriteHeader(500)
				io.WriteString(w, `{"message":"Internal server error"}`)
				return
			}
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such endpoint"}`)
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

	// Verify that start was called
	if !startCalled {
		t.Error("expected POST .../start to be attempted")
	}

	// Verify that the pending record is kept (recovery failed)
	f, st, err = state.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if len(st.Pending) != 1 {
		t.Errorf("expected Pending list to have 1 record (recovery failed), got %d", len(st.Pending))
	}

	// Verify that an event was added with "crash recovery failed"
	if len(st.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(st.Events))
		return
	}
	if st.Events[0].Kind != "recovered" || st.Events[0].Name != "app" {
		t.Errorf("unexpected event: %v", st.Events[0])
	}
	if !strings.Contains(st.Events[0].Message, "crash recovery failed") {
		t.Errorf("event message should contain 'crash recovery failed': %s", st.Events[0].Message)
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

// recoverFake is a swap cut off after the new container started: old-id is
// stopped under a temp name and new-id holds the name app.
func recoverFake(newRunning bool, health string) *dockerFake {
	f := swapFake(newRunning)
	f.containers["app"] = newCtr(true, "")
	f.containers["new-id"] = newCtr(newRunning, health)
	return f
}

func recoverApp(t *testing.T, g *Gate, e state.Entry, digest string) *state.State {
	t.Helper()
	setEntry(t, g, "app", e, state.Pending{Name: "app", OldID: "old-id", TmpName: "app-bosun-x", Digest: digest})
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || len(st.Events) != 1 {
		t.Fatalf("state = %+v, want nothing pending and one event", st)
	}
	return st
}

func TestRecoverRevertsNewThatFailsTheHealthWait(t *testing.T) {
	f := recoverFake(false, "") // running at first look, stopped at the health wait
	g := f.gate(t)
	st := recoverApp(t, g, state.Entry{}, "sha256:d2")
	for _, c := range []string{"DELETE /containers/new-id?force=1&v=0", "POST /containers/old-id/rename?name=app", "POST /containers/old-id/start", retagOld} {
		if !f.called(c) {
			t.Errorf("missing %s; calls: %v", c, f.calls)
		}
	}
	if !strings.Contains(st.Events[0].Message, "old version is back") || !slices.Contains(st.Entry("app").Skip, "sha256:d2") {
		t.Errorf("state = %+v, want the old version back and d2 skipped", st)
	}
}

func TestRecoverKeepsNewThatPassesTheHealthWait(t *testing.T) {
	f := recoverFake(true, "healthy")
	g := f.gate(t)
	st := recoverApp(t, g, state.Entry{}, "sha256:d2")
	if !f.called("DELETE /containers/old-id?force=1&v=0") || !f.called("POST /images/sha256:old/tag?repo=bosun/prev/app&tag=old") {
		t.Errorf("old container not removed or old image not kept; calls: %v", f.calls)
	}
	e := st.Entry("app")
	if !strings.Contains(st.Events[0].Message, "was kept") || e.Prev != "bosun/prev/app:old" || e.UpdatedAt.IsZero() {
		t.Errorf("state = %+v %+v, want the new version kept with a rollback record", st, e)
	}
}

// A cut-off rollback that is kept must skip the version it left, or the next
// round would update straight back to it.
func TestRecoverKeptRollbackSkipsTheVersionItLeft(t *testing.T) {
	f := recoverFake(true, "healthy")
	g := f.gate(t)
	st := recoverApp(t, g, state.Entry{Prev: "bosun/prev/app:abc"}, "")
	e := st.Entry("app")
	if e.Prev != "" || !slices.Contains(e.Skip, "sha256:d1") || !f.called("DELETE /images/bosun/prev/app:abc") {
		t.Errorf("entry = %+v, want d1 skipped and no rollback record; calls: %v", e, f.calls)
	}
}
