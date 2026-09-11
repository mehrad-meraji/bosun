package gate

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
)

func ctr(running bool, restarts int, health string) *docker.Container {
	c := &docker.Container{RestartCount: restarts}
	c.State.Running = running
	c.State.Status = "running"
	if health != "" {
		c.State.Health = &docker.Health{Status: health}
	}
	return c
}

func TestVerdict(t *testing.T) {
	const timeout = 10 * time.Second
	for _, tc := range []struct {
		name    string
		c       *docker.Container
		elapsed time.Duration
		done    bool
		ok      bool
	}{
		{"no healthcheck, still waiting", ctr(true, 0, ""), time.Second, false, false},
		{"no healthcheck, stable until timeout", ctr(true, 0, ""), timeout, true, true},
		{"exited", ctr(false, 0, ""), time.Second, true, false},
		{"restarted once", ctr(true, 1, ""), time.Second, true, false},
		{"healthcheck starting", ctr(true, 0, "starting"), time.Second, false, false},
		{"healthy", ctr(true, 0, "healthy"), time.Second, true, true},
		{"unhealthy", ctr(true, 0, "unhealthy"), time.Second, true, false},
		{"never healthy", ctr(true, 0, "starting"), timeout, true, false},
	} {
		done, err := verdict(tc.c, tc.elapsed, timeout)
		if done != tc.done || (done && (err == nil) != tc.ok) {
			t.Errorf("%s: done=%v err=%v; want done=%v ok=%v", tc.name, done, err, tc.done, tc.ok)
		}
	}
}

func TestTrackable(t *testing.T) {
	for ref, want := range map[string]bool{
		"nginx:1.27":            true,
		"ghcr.io/me/app":        true,
		"sha256:abcdef":         false,
		"nginx@sha256:abcdef":   false,
		"nginx:1.27@sha256:abc": false,
		"":                      false,
	} {
		c := &docker.Container{}
		c.Config.Image = ref
		if _, ok := trackable(c); ok != want {
			t.Errorf("trackable(%q) = %v, want %v", ref, ok, want)
		}
	}
}

func TestDigestsOf(t *testing.T) {
	img := &docker.Image{RepoDigests: []string{"nginx@sha256:aa", "localhost:5000/nginx@sha256:bb"}}
	got := digestsOf(img)
	if len(got) != 2 || got[0] != "sha256:aa" || got[1] != "sha256:bb" {
		t.Errorf("digestsOf = %v", got)
	}
}

func TestTimeoutOf(t *testing.T) {
	c := &docker.Container{}
	if timeoutOf(c) != 60*time.Second {
		t.Error("default must be 60s")
	}
	c.Config.Labels = map[string]string{LabelTimeout: "2m"}
	if timeoutOf(c) != 2*time.Minute {
		t.Error("label must win")
	}
	c.Config.Labels[LabelTimeout] = "soon"
	if timeoutOf(c) != 60*time.Second {
		t.Error("bad label must fall back to 60s")
	}
}

func TestWatchedBadName(t *testing.T) {
	// Test that bad names are refused without touching Docker (D is nil)
	g := &Gate{D: nil, Dir: ""}
	for _, badName := range []string{
		"",                      // empty
		"_web",                  // starts with underscore
		"-web",                  // starts with dash
		".web",                  // starts with dot
		"web/name",              // contains slash
		"web?name",              // contains question mark
		strings.Repeat("a", 129), // too long
	} {
		_, _, err := g.watched(context.Background(), badName)
		if err == nil || !isRefused(err) {
			t.Errorf("watched(%q) should refuse, got %v", badName, err)
		}
	}
}

func TestWatchedNameIDMismatch(t *testing.T) {
	// Test that using a container ID when a name is required is refused
	g := fakeGate(t, func(w http.ResponseWriter, r *http.Request) {
		// The request might be for abc123 (ID prefix used as name)
		if r.URL.Path == "/v1.44/containers/abc123/json" {
			// Return a container with a different name and required labels
			resp := docker.Container{
				ID:   "abc123def456",
				Name: "/web",
				Config: docker.ContainerConfig{
					Image: "nginx:1.27",
					Labels: map[string]string{LabelEnable: "true"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	// Using the ID prefix should be refused (doesn't match the actual name "web")
	_, _, err := g.watched(context.Background(), "abc123")
	if err == nil || !isRefused(err) {
		t.Errorf("watched(id prefix) should refuse, got %v", err)
	}
	// Verify the error is specifically about the name/ID mismatch, not the label
	if !strings.Contains(err.Error(), "not a container name") {
		t.Errorf("expected 'not a container name' in error, got %v", err)
	}
}

// fakeGate creates a Gate with a fake Docker server for testing.
func fakeGate(t *testing.T, h http.HandlerFunc) *Gate {
	t.Helper()
	dir, err := os.MkdirTemp("", "bg")
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
	return &Gate{D: docker.New(p), Dir: dir}
}

func isRefused(err error) bool {
	_, ok := err.(*RefusedError)
	return ok
}
