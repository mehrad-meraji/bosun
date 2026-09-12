package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/gate"
)

// tokenFile writes a token file and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testClient points a Client at s with retries made instant.
func testClient(t *testing.T, s *httptest.Server) *Client {
	t.Helper()
	c, err := New(s.URL, tokenFile(t, "secret-token"), "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewIsOffWithoutAURL(t *testing.T) {
	c, err := New("", "/nope", "worker-1", false)
	if c != nil || err != nil {
		t.Fatalf("New(\"\") = %v, %v, want nil, nil: the link is off", c, err)
	}
}

func TestNewRefusesPlainHTTPAndBadSettings(t *testing.T) {
	tok := tokenFile(t, "t")
	for _, tc := range []struct{ name, url, token string }{
		{"plain http", "http://server", tok},
		{"no scheme", "server:8080", tok},
		{"missing token file", "https://server", filepath.Join(t.TempDir(), "gone")},
		{"empty token file", "https://server", tokenFile(t, "")},
	} {
		if _, err := New(tc.url, tc.token, "h", false); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	if _, err := New("https://server", tok, "", false); err == nil {
		t.Error("no host name: want an error, or every event goes out with an empty host")
	}
	if _, err := New("http://server", tok, "h", true); err != nil {
		t.Errorf("plain http with insecure = %v, want ok", err)
	}
}

func TestSendEvent(t *testing.T) {
	var body []byte
	var auth, ctype string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/events" {
			t.Errorf("got %s %s, want POST /events", r.Method, r.URL.Path)
		}
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
	}))
	defer s.Close()

	c := testClient(t, s)
	err := c.Send(context.Background(), Event{Type: EventUpdateDone, Container: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret-token" || ctype != "application/json" {
		t.Errorf("headers = %q, %q", auth, ctype)
	}
	var got Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != 1 || got.Version != Version || got.Host != "worker-1" || got.ID == "" || got.Time.IsZero() {
		t.Errorf("event = %+v, want the fixed fields filled in", got)
	}
	// Two events never share an id: the server drops repeats by id.
	first := got.ID
	if err := c.Send(context.Background(), Event{Type: EventUpdateDone}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(body, &got)
	if got.ID == first {
		t.Error("two events share an id")
	}
}

func TestEventJSONIsTheShapeTheServerExpects(t *testing.T) {
	ev := Event{
		Schema:     Schema,
		ID:         "0f8c2c1e-5b7a-4d0e-9a51-3c2d7e6f1a90",
		Time:       time.Date(2026, 9, 11, 4, 1, 33, 0, time.UTC),
		Host:       "worker-1",
		Version:    "0.2.0",
		Type:       EventUpdateRolledBack,
		Container:  "redis",
		Image:      "redis:7.4",
		FromDigest: "sha256:b20c",
		ToDigest:   "sha256:9ae1",
		DowntimeMs: 38000,
		Steps: []gate.Step{
			{Name: "pull", Status: "ok", Ms: 6200},
			{Name: "backup", Status: "skipped", Detail: "bosun.backup is off"},
		},
		Reason: "health check failed",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,"id":"0f8c2c1e-5b7a-4d0e-9a51-3c2d7e6f1a90","time":"2026-09-11T04:01:33Z","host":"worker-1","bosun_version":"0.2.0","type":"update.rolled_back","container":"redis","image":"redis:7.4","from_digest":"sha256:b20c","to_digest":"sha256:9ae1","downtime_ms":38000,"steps":[{"name":"pull","status":"ok","ms":6200},{"name":"backup","status":"skipped","detail":"bosun.backup is off"}],"reason":"health check failed"}`
	if string(b) != want {
		t.Errorf("event JSON changed.\ngot:  %s\nwant: %s", b, want)
	}
}

func TestSendRetriesThenGivesUp(t *testing.T) {
	var tries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()

	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryWaits = old }()

	c := testClient(t, s)
	if err := c.Send(context.Background(), Event{Type: EventWarning}); err == nil {
		t.Fatal("want an error after the last try")
	}
	if n := tries.Load(); n != 4 {
		t.Errorf("tries = %d, want 4 (one, then three more)", n)
	}
}

func TestSendDoesNotRetryABadTokenAndNeverLeaksIt(t *testing.T) {
	var tries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		http.Error(w, "bad token secret-token", http.StatusUnauthorized)
	}))
	defer s.Close()

	c := testClient(t, s)
	err := c.Send(context.Background(), Event{Type: EventWarning})
	if err == nil {
		t.Fatal("want an error")
	}
	if tries.Load() != 1 {
		t.Errorf("tries = %d, want 1: a bad token does not fix itself", tries.Load())
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the token must never be in an error: %v", err)
	}
}

func TestSendStopsWhenTheContextEnds(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()
	old := retryWaits
	retryWaits = []time.Duration{time.Hour}
	defer func() { retryWaits = old }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := testClient(t, s)
	if err := c.Send(ctx, Event{Type: EventWarning}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
