package updater

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func TestRunCommandCheck(t *testing.T) {
	u, g, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "done" || len(g.updates) != 1 {
		t.Errorf("status = %q, updates = %v, want done and one update", status, g.updates)
	}
}

func TestRunCommandCheckIsBusy(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{digest: "sha256:new"})
	u.mu.Lock() // a round is running
	defer u.mu.Unlock()
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "busy" || detail == "" {
		t.Errorf("status = %q, %q, want busy with a reason", status, detail)
	}
}

func TestRunCommandSkipClear(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.cleared = gate.SkipClearResult{Cleared: 2, Message: "app: skip list cleared (2)"}
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"})
	if status != "done" || detail == "" || g.skipCleared != "app" {
		t.Errorf("status = %q, %q, cleared %q, want done for app", status, detail, g.skipCleared)
	}
}

func TestRunCommandSkipClearBusyAndRefused(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.clearErr = state.ErrBusy // the gate's own error; over the socket it is a 409, which gate.IsBusy also reads
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"}); status != "busy" {
		t.Errorf("status = %q, want busy", status)
	}
	g.clearErr = &gate.RefusedError{Msg: "bad container name"}
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c2", Type: control.CmdSkipClear, Container: "app"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
	g.clearErr = errors.New("disk on fire")
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c3", Type: control.CmdSkipClear, Container: "app"}); status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestRunCommandUnknownType(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{})
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: "reboot"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
}

// controlClient is a link to s, with a token file like the real one.
func controlClient(t *testing.T, url string) *control.Client {
	t.Helper()
	p := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(p, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := control.New(url, p, "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// linked is an updater with the control link on and a queue the test reads
// the events out of, in place of the sender goroutine.
func linked(t *testing.T, url string, ws []gate.Watched, reg fakeReg) (*Updater, *fakeGate) {
	t.Helper()
	u, g, _ := setup(ws, reg)
	u.Control = controlClient(t, url)
	u.evs = make(chan control.Event, eventQueue)
	return u, g
}

// quiet sends the log somewhere the test can read and no one else can see.
func quiet(t *testing.T) *lockedWriter {
	t.Helper()
	l := &lockedWriter{mu: &sync.Mutex{}, w: &bytes.Buffer{}}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// lockedWriter makes the log buffer safe to read while the loop writes.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) read() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}

// nextResult waits for the next command.result off the queue, past the
// events a round of its own makes.
func nextResult(t *testing.T, u *Updater) control.Event {
	t.Helper()
	for {
		select {
		case ev := <-u.evs:
			if ev.Type == control.EventCommandResult {
				return ev
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no command result came out of the queue")
			return control.Event{}
		}
	}
}

// serveOnce answers the first poll with body and every later one with an
// empty list. It also counts the polls.
func serveOnce(body string) (http.HandlerFunc, *atomic.Int64) {
	var polls atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, `[]`)
	}, &polls
}

// run starts the poll loop and returns a stop function that cancels it and
// waits for it to return.
func runLoop(t *testing.T, u *Updater) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Commands(ctx, 5*time.Millisecond) }()
	return ctx, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Commands returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Commands did not return after the context ended")
		}
	}
}

func TestCommandsLoopRunsACheck(t *testing.T) {
	quiet(t)
	h, _ := serveOnce(`[{"id":"c1","type":"check"}]`)
	s := httptest.NewServer(h)
	defer s.Close()
	u, g := linked(t, s.URL, web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	_, stop := runLoop(t, u)
	ev := nextResult(t, u)
	stop()
	if ev.Type != control.EventCommandResult || ev.CommandID != "c1" || ev.Status != "done" {
		t.Errorf("event = %+v, want a done result for c1", ev)
	}
	if len(g.updates) != 1 {
		t.Errorf("updates = %v, want one round", g.updates)
	}
}

// A server that is down or talking junk must not stop the loop: the next
// good reply is still handled.
func TestCommandsLoopSurvivesABadReply(t *testing.T) {
	buf := quiet(t)
	var polls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1:
			io.WriteString(w, `not json at all`)
		case 2:
			http.Error(w, "go away", http.StatusInternalServerError)
		case 3:
			io.WriteString(w, `[{"id":"c1","type":"check"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer s.Close()
	u, g := linked(t, s.URL, web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	_, stop := runLoop(t, u)
	ev := nextResult(t, u)
	stop()
	if ev.Status != "done" || ev.CommandID != "c1" {
		t.Errorf("event = %+v, want the command after the bad replies to still run", ev)
	}
	if len(g.updates) != 1 {
		t.Errorf("updates = %v, want one round", g.updates)
	}
	if !strings.Contains(buf.read(), "read commands") {
		t.Errorf("a bad reply must be logged: %q", buf.read())
	}
}

// The end of the context is not a failure: it must not log a failed read.
func TestCommandsLoopEndsQuietly(t *testing.T) {
	buf := quiet(t)
	got := make(chan struct{})
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
		<-block // hold the poll open until the test cancels
	}))
	defer func() { close(block); s.Close() }()
	u, _ := linked(t, s.URL, nil, fakeReg{})
	_, stop := runLoop(t, u)
	<-got
	stop() // cancels, then waits for Commands to return
	if strings.Contains(buf.read(), "read commands") {
		t.Errorf("the end of the context must not log a failed read: %q", buf.read())
	}
}

// The busy contract: a command with no terminal result comes back and runs
// once the round lock is free. The server drops a failed command but retries
// a busy one, so a busy id must never be remembered.
func TestCommandsLoopRetriesABusyCheck(t *testing.T) {
	quiet(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"c1","type":"check"}]`)
	}))
	defer s.Close()
	u, g := linked(t, s.URL, web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	u.mu.Lock() // a round is running
	_, stop := runLoop(t, u)
	if ev := nextResult(t, u); ev.Status != "busy" || ev.CommandID != "c1" {
		t.Fatalf("first event = %+v, want busy for c1", ev)
	}
	u.mu.Unlock()
	ev := nextResult(t, u)
	for ev.Status == "busy" { // the lock may free between two polls
		ev = nextResult(t, u)
	}
	stop()
	if ev.Status != "done" || ev.CommandID != "c1" {
		t.Errorf("event = %+v, want the same command run after the lock was free", ev)
	}
	if len(g.updates) != 1 {
		t.Errorf("updates = %v, want exactly one round", g.updates)
	}
}

// A reply with many checks asks for one thing: a round now. Running each of
// them would let one reply start up to 100 rounds.
func TestRunReplyCollapsesChecks(t *testing.T) {
	quiet(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	}))
	defer s.Close()
	u, g := linked(t, s.URL, web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	u.runReply(context.Background(), []control.Command{
		{ID: "c1", Type: control.CmdCheck},
		{ID: "c2", Type: control.CmdCheck},
		{ID: "c3", Type: control.CmdCheck},
	}, nil)
	if len(g.updates) != 1 {
		t.Errorf("updates = %v, want one round for three checks", g.updates)
	}
	for _, want := range []string{"c1", "c2", "c3"} {
		ev := nextResult(t, u)
		if ev.CommandID != want || ev.Status != "done" {
			t.Errorf("event = %+v, want a done result for %s", ev, want)
		}
	}
}

// A busy command is left unmarked, so the server's next poll offers it
// again; a terminal one is marked and dropped.
func TestRunReplyMarksOnlyTerminalResults(t *testing.T) {
	quiet(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"c1","type":"check"}]`)
	}))
	defer s.Close()
	u, _ := linked(t, s.URL, nil, fakeReg{})
	c1 := []control.Command{{ID: "c1", Type: control.CmdCheck}}

	u.mu.Lock() // a round is running: the result is busy
	u.runReply(context.Background(), c1, nil)
	u.mu.Unlock()
	if ev := nextResult(t, u); ev.Status != "busy" {
		t.Fatalf("event = %+v, want busy", ev)
	}
	cmds, _, err := u.Control.Commands(context.Background())
	if err != nil || len(cmds) != 1 {
		t.Fatalf("poll after busy = %v, %v, want the command offered again", cmds, err)
	}

	u.runReply(context.Background(), c1, nil)
	if ev := nextResult(t, u); ev.Status != "done" {
		t.Fatalf("event = %+v, want done", ev)
	}
	cmds, _, err = u.Control.Commands(context.Background())
	if err != nil || len(cmds) != 0 {
		t.Errorf("poll after done = %v, %v, want the command dropped", cmds, err)
	}
}
