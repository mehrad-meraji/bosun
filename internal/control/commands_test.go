package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseCommands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantOK     []string // command IDs kept
		wantRefuse []string // command IDs refused
		wantErr    bool
	}{
		{name: "both commands", body: `[{"id":"a","type":"check"},{"id":"b","type":"skip_clear","container":"nginx"}]`, wantOK: []string{"a", "b"}},
		{name: "empty list", body: `[]`},
		{name: "unknown command", body: `[{"id":"a","type":"reboot"}]`, wantRefuse: []string{"a"}},
		{name: "unknown field", body: `[{"id":"a","type":"check","image":"evil"}]`, wantErr: true},
		{name: "skip_clear without a container", body: `[{"id":"a","type":"skip_clear"}]`, wantRefuse: []string{"a"}},
		{name: "skip_clear with a bad container", body: `[{"id":"a","type":"skip_clear","container":"../etc"}]`, wantRefuse: []string{"a"}},
		{name: "check with a container", body: `[{"id":"a","type":"check","container":"nginx"}]`, wantRefuse: []string{"a"}},
		{name: "no id", body: `[{"type":"check"}]`},
		{name: "bad id", body: `[{"id":"a b","type":"check"}]`},
		{name: "not a list", body: `{"id":"a","type":"check"}`, wantErr: true},
		{name: "junk", body: `not json`, wantErr: true},
		{name: "two objects", body: `[] []`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds, refs, err := parse([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			var ok, refused []string
			for _, c := range cmds {
				ok = append(ok, c.ID)
			}
			for _, r := range refs {
				refused = append(refused, r.ID)
			}
			if strings.Join(ok, ",") != strings.Join(tc.wantOK, ",") {
				t.Errorf("kept = %v, want %v", ok, tc.wantOK)
			}
			if strings.Join(refused, ",") != strings.Join(tc.wantRefuse, ",") {
				t.Errorf("refused = %v, want %v", refused, tc.wantRefuse)
			}
			for _, r := range refs {
				if r.Reason == "" {
					t.Error("a refusal must say why")
				}
			}
		})
	}
}

func TestParseRefusesATooLongList(t *testing.T) {
	var cmds []Command
	for i := 0; i < maxCommands+1; i++ {
		cmds = append(cmds, Command{ID: string(rune('a'+i%26)) + string(rune('a'+i/26)), Type: CmdCheck})
	}
	b, _ := json.Marshal(cmds)
	if _, _, err := parse(b); err == nil {
		t.Error("want an error for a list over the cap")
	}
}

func TestCommandsDropsRepeats(t *testing.T) {
	body := `[{"id":"a","type":"check"}]`
	var host string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/commands" {
			t.Errorf("got %s %s, want GET /commands", r.Method, r.URL.Path)
		}
		host = r.URL.Query().Get("host")
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("auth = %q", got)
		}
		io.WriteString(w, body)
	}))
	defer s.Close()

	c := testClient(t, s)
	cmds, _, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 1 {
		t.Fatalf("first poll = %v, %v, want one command", cmds, err)
	}
	if host != "worker-1" {
		t.Errorf("host = %q, want worker-1", host)
	}
	cmds, refs, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 0 || len(refs) != 0 {
		t.Errorf("second poll = %v, %v, %v, want the repeat dropped and not reported", cmds, refs, err)
	}
}

func TestCommandsForgetsOldIDs(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"a","type":"check"}]`)
	}))
	defer s.Close()
	c := testClient(t, s)
	if _, _, err := c.Commands(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.seen["a"] = time.Now().Add(-25 * time.Hour) // older than the window
	cmds, _, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 1 {
		t.Errorf("after 25 hours = %v, %v, want the command again", cmds, err)
	}
}

func TestCommandsFailsOnABadStatus(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no secret-token for you", http.StatusUnauthorized)
	}))
	defer s.Close()
	_, _, err := testClient(t, s).Commands(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the token must never be in an error: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(`[{"id":"a","type":"check"}]`))
	f.Add([]byte(`[{"id":"a","type":"skip_clear","container":"nginx"}]`))
	f.Add([]byte(`[{"id":"a","type":"skip_clear","container":"../../etc/passwd"}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		cmds, refs, err := parse(b)
		if err != nil {
			if cmds != nil || refs != nil {
				t.Error("an error must take nothing with it")
			}
			return
		}
		for _, c := range cmds {
			if c.Type != CmdCheck && c.Type != CmdSkipClear {
				t.Errorf("kept an unknown command %q", c.Type)
			}
			if !idRE.MatchString(c.ID) {
				t.Errorf("kept a bad id %q", c.ID)
			}
			if c.Type == CmdSkipClear && !nameRE.MatchString(c.Container) {
				t.Errorf("kept a bad container %q", c.Container)
			}
			if c.Type == CmdCheck && c.Container != "" {
				t.Errorf("kept a check with a container %q", c.Container)
			}
		}
	})
}
