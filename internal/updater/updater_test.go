package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

type fakeGate struct {
	ws      []gate.Watched
	updates []string
	events  []state.Event

	cleared     gate.SkipClearResult
	clearErr    error
	skipCleared string
}

func (f *fakeGate) List(context.Context) ([]gate.Watched, error) { return f.ws, nil }
func (f *fakeGate) Update(_ context.Context, name, digest, auth string) (gate.Result, error) {
	f.updates = append(f.updates, name+" "+digest+" "+auth)
	return gate.Result{Status: gate.StatusDone, Message: name + ": now running"}, nil
}
func (f *fakeGate) Events(context.Context) ([]state.Event, error) {
	evs := f.events
	f.events = nil
	return evs, nil
}

func (f *fakeGate) SkipClear(_ context.Context, name string) (gate.SkipClearResult, error) {
	f.skipCleared = name
	return f.cleared, f.clearErr
}

type fakeReg struct {
	digest string
	err    error
}

func (r fakeReg) Digest(context.Context, string) (string, error) { return r.digest, r.err }
func (r fakeReg) Auth(string) (string, error)                    { return "AUTH", nil }

func setup(ws []gate.Watched, reg fakeReg) (*Updater, *fakeGate, *[]string) {
	g := &fakeGate{ws: ws}
	var notes []string
	return &Updater{Gate: g, Reg: reg, Notify: func(s string) { notes = append(notes, s) }}, g, &notes
}

func web(mode string, digests, skip []string) []gate.Watched {
	return []gate.Watched{{Name: "web", Ref: "nginx:1.27", Mode: mode, Digests: digests, Skip: skip}}
}

func TestRoundUpdatesNewDigest(t *testing.T) {
	u, g, notes := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	if _, err := u.Round(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.updates, []string{"web sha256:new AUTH"}) {
		t.Errorf("updates = %v", g.updates)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "now running") {
		t.Errorf("notes = %v", *notes)
	}
}

func TestRoundSkipsUpToDateAndSkipped(t *testing.T) {
	for _, ws := range [][]gate.Watched{
		web("update", []string{"sha256:new"}, nil),
		web("update", []string{"sha256:old"}, []string{"sha256:new"}),
	} {
		u, g, notes := setup(ws, fakeReg{digest: "sha256:new"})
		u.Round(context.Background(), false)
		if len(g.updates) != 0 || len(*notes) != 0 {
			t.Errorf("%+v: updates=%v notes=%v, want none", ws, g.updates, *notes)
		}
	}
}

func TestRoundNotifyModeTellsOnce(t *testing.T) {
	u, g, notes := setup(web("notify", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	u.Round(context.Background(), false)
	u.Round(context.Background(), false)
	if len(g.updates) != 0 {
		t.Errorf("notify mode updated: %v", g.updates)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "ready") {
		t.Errorf("notes = %v, want one 'ready' note", *notes)
	}
}

func TestRoundRegistryFailureNotesOnThirdRound(t *testing.T) {
	u, _, notes := setup(web("update", nil, nil), fakeReg{err: errors.New("timeout")})
	for i := 0; i < 4; i++ {
		u.Round(context.Background(), false)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "3 rounds") {
		t.Errorf("notes = %v, want one note after the third failure", *notes)
	}
}

func TestRoundDryRunChangesNothing(t *testing.T) {
	u, g, notes := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	lines, err := u.Round(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.updates) != 0 || len(*notes) != 0 {
		t.Errorf("dry run acted: updates=%v notes=%v", g.updates, *notes)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "would update") {
		t.Errorf("lines = %v", lines)
	}
}

func TestRoundSendsGateEvents(t *testing.T) {
	u, g, notes := setup(nil, fakeReg{})
	g.events = []state.Event{{Message: "web: the old version is back"}}
	u.Round(context.Background(), false)
	if len(*notes) != 1 || (*notes)[0] != "web: the old version is back" {
		t.Errorf("notes = %v", *notes)
	}
}

func TestRoundBusy(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{})
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, err := u.Round(context.Background(), false); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestLoadURLs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notify.txt")
	os.WriteFile(p, []byte("# team\nslack://a/b/c\n\n  ntfy://ntfy.sh/topic  \n"), 0o600)
	got, err := LoadURLs(p)
	if err != nil || !reflect.DeepEqual(got, []string{"slack://a/b/c", "ntfy://ntfy.sh/topic"}) {
		t.Fatalf("LoadURLs = %v, %v", got, err)
	}
	if got, err := LoadURLs(filepath.Join(t.TempDir(), "none")); err != nil || got != nil {
		t.Fatalf("missing file: %v, %v; want no URLs and no error", got, err)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: `sending POST request to "https://hooks.slack.com/services/T1/B2/SECRET": dial tcp: timeout`,
			want:  "dial tcp: timeout",
		},
		{
			input: `locating service for URL "slack://tok/en": bad`,
			want:  "bad",
		},
	}
	for _, tt := range tests {
		got := redact(tt.input)
		if !strings.Contains(got, "SECRET") && !strings.Contains(got, "slack://tok") && strings.Contains(got, tt.want) {
			continue
		}
		t.Errorf("redact(%q) = %q, missing expected %q or contains redacted secrets", tt.input, got, tt.want)
	}
}

// controlFake is a control server that records the events it is sent.
func controlFake(t *testing.T) (*control.Client, func() []control.Event) {
	t.Helper()
	var mu sync.Mutex
	var got []control.Event
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev control.Event
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	t.Cleanup(s.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := control.New(s.URL, tok, "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}
	// events go out in another goroutine; wait for n of them
	return c, func() []control.Event {
		for i := 0; i < 100; i++ {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]control.Event(nil), got...)
	}
}

func TestRoundSendsAnEvent(t *testing.T) {
	u, _, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)

	if _, err := u.Round(ctx, false); err != nil {
		t.Fatal(err)
	}
	evs := events()
	if len(evs) != 1 || evs[0].Type != control.EventUpdateDone || evs[0].Container != "web" {
		t.Fatalf("events = %+v, want one update.done for web", evs)
	}
	if evs[0].Host != "worker-1" || evs[0].Schema != control.Schema {
		t.Errorf("event = %+v, want the fixed fields filled in", evs[0])
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	u, _, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)
	if _, err := u.Round(ctx, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if evs := events(); len(evs) != 0 {
		t.Errorf("a dry run must send nothing: %+v", evs)
	}
}

func TestRoundWorksWithTheLinkOff(t *testing.T) {
	u, g, notes := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	u.StartEvents(context.Background()) // no Control: must not panic and must start nothing
	if _, err := u.Round(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(g.updates) != 1 || len(*notes) != 1 {
		t.Errorf("the round must work the same with the link off: %v %v", g.updates, *notes)
	}
}

func TestEmitDropsWhenTheQueueIsFull(t *testing.T) {
	u := &Updater{Control: &control.Client{}} // never read: emit only fills the queue
	u.evs = make(chan control.Event, 1)
	u.emit(control.Event{Type: control.EventWarning})
	u.emit(control.Event{Type: control.EventWarning}) // must not block
	if len(u.evs) != 1 {
		t.Errorf("queue = %d, want the second event dropped", len(u.evs))
	}
}

func TestGateEventsGoOutAsEvents(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{digest: "sha256:new"})
	g.events = []state.Event{{Kind: "recovered", Name: "app", Message: "app: the old version is back"}}
	c, events := controlFake(t)
	u.Control = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.StartEvents(ctx)
	if _, err := u.Round(ctx, false); err != nil {
		t.Fatal(err)
	}
	evs := events()
	if len(evs) != 1 || evs[0].Type != control.EventRecovery {
		t.Fatalf("events = %+v, want one recovery event", evs)
	}
}

// queued turns the link on with a queue the test reads, and no server: no
// event leaves the machine.
func queued(t *testing.T, u *Updater) {
	t.Helper()
	u.Control = controlClient(t, "https://control.invalid")
	u.evs = make(chan control.Event, eventQueue)
}

// drain reads every event waiting in the queue.
func drain(u *Updater) []control.Event {
	var out []control.Event
	for {
		select {
		case ev := <-u.evs:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// The gate's queued events were not caused by the command that happened to
// run the round: a crash recovery from hours earlier is its own news.
func TestGateEventsCarryNoCommandID(t *testing.T) {
	u, g, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	queued(t, u)
	g.events = []state.Event{{Kind: "recovered", Name: "app", Message: "app: the old version is back"}}
	if _, err := u.round(context.Background(), false, "cmd-1"); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, ev := range drain(u) {
		switch ev.Type {
		case control.EventRecovery:
			saw = true
			if ev.CommandID != "" {
				t.Errorf("a gate event must carry no command id: %+v", ev)
			}
		case control.EventUpdateDone:
			if ev.CommandID != "cmd-1" {
				t.Errorf("the update the command caused must carry its id: %+v", ev)
			}
		}
	}
	if !saw {
		t.Error("no recovery event was sent")
	}
}

// An event's time is when it happened, not when a slow server finally took
// it. A gate event keeps the gate's own time.
func TestEmitStampsTheTimeItHappened(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{})
	queued(t, u)
	before := time.Now().UTC()
	u.emit(resultEvent("c1", "done", "checked 0 containers"))
	when := time.Date(2026, 9, 11, 4, 0, 0, 0, time.UTC)
	u.emit(gateEvent(state.Event{Time: when, Kind: "recovered", Name: "app", Message: "back"}, ""))
	evs := drain(u)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want two", evs)
	}
	if evs[0].Time.Before(before) || evs[0].Time.After(time.Now().UTC()) {
		t.Errorf("time = %v, want the moment it was queued", evs[0].Time)
	}
	if !evs[1].Time.Equal(when) {
		t.Errorf("time = %v, want the gate's own time %v", evs[1].Time, when)
	}
}
