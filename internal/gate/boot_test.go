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
	b, _ := json.Marshal(updaterBody(self, "/run/bosun", ""))
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

// A --with-data rollback cut off with the newer version's mounts already
// holding restored (old) data must never be started back up.
func TestRecoverKeepStoppedNeverRestartsRestoredData(t *testing.T) {
	f := recoverFake(false, "") // new container running at first look, stopped at the health wait
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{}, state.Pending{Name: "app", OldID: "old-id", TmpName: "app-bosun-x", KeepStopped: true})
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
	if f.called("POST /containers/old-id/start") {
		t.Errorf("must never start a container whose mounts hold restored backup data; calls: %v", f.calls)
	}
	if !strings.Contains(st.Events[0].Message, "is stopped") {
		t.Errorf("event must say the container is stopped: %s", st.Events[0].Message)
	}
	if f.called(retagOld) {
		t.Errorf("data was restored, so the tag must stay on the old image; calls: %v", f.calls)
	}
}

// When a --with-data recovery itself fails (the rename back to the app's
// name fails), the event must never tell the user to start the container:
// its mounts hold restored (old) data under the newer image.
func TestRecoverKeepStoppedFailureDoesNotSayStartIt(t *testing.T) {
	f := recoverFake(false, "") // new container exists but unhealthy
	f.fail["POST /containers/old-id/rename"] = true
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{}, state.Pending{Name: "app", OldID: "old-id", TmpName: "app-bosun-x", KeepStopped: true})
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 1 || len(st.Events) != 1 {
		t.Fatalf("state = %+v, want the pending record kept and one event", st)
	}
	msg := st.Events[0].Message
	if !strings.Contains(msg, "do not start it") || strings.Contains(msg, "and start it") {
		t.Errorf("event must say not to start it, and never tell the user to start it: %s", msg)
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

// A --with-data rollback that is cut off after the old version came up
// healthy is finished: the DataRestored flag must be cleared, or Update and
// Rollback both refuse the app for good.
func TestRecoverKeptRollbackClearsDataRestored(t *testing.T) {
	f := recoverFake(true, "healthy")
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{Prev: "bosun/prev/app:abc", DataRestored: true},
		state.Pending{Name: "app", OldID: "old-id", TmpName: "app-bosun-x", KeepStopped: true})
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := st.Entry("app"); e.DataRestored {
		t.Errorf("entry = %+v, want DataRestored cleared after a kept rollback", e)
	}
}

func TestUpdaterBodyCarriesTheHost(t *testing.T) {
	self := &docker.Container{Image: "sha256:img"}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun", "worker-1"))
	if !strings.Contains(string(b), `"BOSUN_HOST=worker-1"`) {
		t.Errorf("updater body lacks the host: %s", b)
	}
}

func TestUpdaterBodyKeepsTheUsersHost(t *testing.T) {
	self := &docker.Container{Image: "sha256:img"}
	self.Config.Env = []string{"BOSUN_HOST=mine"}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun", ""))
	s := string(b)
	if !strings.Contains(s, `"BOSUN_HOST=mine"`) || strings.Count(s, "BOSUN_HOST=") != 1 {
		t.Errorf("want the user's host once and only once: %s", s)
	}
}

// createUpdater is the call that makes the updater container.
const createUpdater = "POST /containers/create?name=bosun-updater"

// spawnFake is a gate that can find itself, has no old updater to remove,
// and whose GET /info fails: Docker gives no host name.
func spawnFake(t *testing.T) (*dockerFake, *Gate) {
	t.Helper()
	f := &dockerFake{
		containers: map[string]string{"self-id": `{"Id":"self-id","Name":"/bosun-gate","Image":"sha256:img","Config":{"Env":[]},"HostConfig":{},"Mounts":[]}`},
		images:     map[string]string{},
		fail:       map[string]bool{"GET /info": true},
		list:       `[]`,
	}
	g := f.gate(t)
	g.SelfID = "self-id"
	return f, g
}

// Turning on an optional reporting feature must never stop all updating:
// the updater refuses to start without a host name, so the gate says so
// instead of starting a container that crash-loops.
func TestSpawnUpdaterFailsWithoutAHostWhenTheLinkIsOn(t *testing.T) {
	f, g := spawnFake(t)
	g.ControlLink = true
	err := g.SpawnUpdater(context.Background())
	if err == nil {
		t.Fatal("want an error when the link is on and Docker gives no host name")
	}
	if !strings.Contains(err.Error(), "BOSUN_HOST") || !strings.Contains(err.Error(), "BOSUN_CONTROL_URL") {
		t.Errorf("error must say what to do next: %v", err)
	}
	if f.called(createUpdater) {
		t.Errorf("no updater must be created; calls: %v", f.calls)
	}
}

// With the link off the host is unused, so a missing host name is only a
// log line and updating carries on.
func TestSpawnUpdaterCarriesOnWithoutAHostWhenTheLinkIsOff(t *testing.T) {
	f, g := spawnFake(t)
	if err := g.SpawnUpdater(context.Background()); err != nil {
		t.Fatalf("SpawnUpdater = %v, want the updater started anyway", err)
	}
	if !f.called(createUpdater) || !f.called("POST /containers/new-id/start") {
		t.Errorf("the updater must still be created and started; calls: %v", f.calls)
	}
}

// Crash recovery throws away an unhealthy new container too. With
// bosun.logs=true its last output waits in the state file until the updater
// collects it.
func TestRecoverKeepsTheFailedLogs(t *testing.T) {
	f := recoverFake(false, "") // new container exists, fails the health wait
	withLogs(f, "boom: migration failed\n")
	g := f.gate(t)
	st := recoverApp(t, g, state.Entry{}, "sha256:d2")
	if st.Events[0].Logs != "boom: migration failed" {
		t.Errorf("event logs = %q, want the tail of the failed container", st.Events[0].Logs)
	}
	if !f.calledAfter(logsCall, "DELETE /containers/new-id?force=1&v=0") {
		t.Errorf("logs must be read before the new container is removed; calls: %v", f.calls)
	}
}

func TestRecoverSendsNoLogsWithoutTheLabel(t *testing.T) {
	f := recoverFake(false, "")
	g := f.gate(t)
	st := recoverApp(t, g, state.Entry{}, "sha256:d2")
	if st.Events[0].Logs != "" || f.called(logsCall) {
		t.Errorf("logs = %q, calls %v; want none without the label", st.Events[0].Logs, f.calls)
	}
}
