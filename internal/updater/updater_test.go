package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

type fakeGate struct {
	ws      []gate.Watched
	updates []string
	events  []state.Event
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
