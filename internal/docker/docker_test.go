package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake serves h on a unix socket and returns a client for it.
// ponytail: os.MkdirTemp, not t.TempDir, because macOS caps socket paths at 104 bytes.
func fake(t *testing.T, h http.HandlerFunc) *Client {
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
	return New(p)
}

func TestPullReadsErrorsInsideTheStream(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1.44/images/create" || q.Get("fromImage") != "localhost:5000/app" ||
			q.Get("tag") != "2" || r.Header.Get("X-Registry-Auth") != "abc" {
			t.Errorf("unexpected request: %s %v", r.URL, r.Header)
		}
		io.WriteString(w, "{\"status\":\"Pulling\"}\n{\"error\":\"manifest unknown\"}\n")
	})
	err := c.Pull(context.Background(), "localhost:5000/app:2", "abc")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("want the stream error, got %v", err)
	}
}

func TestPullOK(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{\"status\":\"Pulling\"}\n{\"status\":\"Done\"}\n")
	})
	if err := c.Pull(context.Background(), "nginx", ""); err != nil {
		t.Fatal(err)
	}
}

func TestNotModifiedIsOK(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotModified) })
	if err := c.Stop(context.Background(), "abc"); err != nil {
		t.Fatalf("304 on stop must be fine, got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: x"}`)
	})
	_, err := c.Inspect(context.Background(), "x")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestInspectKeepsRawJSON(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Id":"abc","Name":"/web","HostConfig":{"Privileged":false},"Config":{"Labels":{"bosun.enable":"true"}}}`)
	})
	ct, err := c.Inspect(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if ct.Name != "/web" || ct.Config.Labels["bosun.enable"] != "true" {
		t.Fatalf("bad parse: %+v", ct)
	}
	if !strings.Contains(string(ct.Raw), `"HostConfig"`) {
		t.Fatalf("Raw lost fields: %s", ct.Raw)
	}
}

func TestRemoveKeepsVolumes(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Query().Get("v") != "0" || r.URL.Query().Get("force") != "1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Remove(context.Background(), "abc", true); err != nil {
		t.Fatal(err)
	}
}

func TestSplitRef(t *testing.T) {
	for _, tc := range []struct{ in, repo, tag string }{
		{"nginx", "nginx", "latest"},
		{"nginx:1.27", "nginx", "1.27"},
		{"localhost:5000/app", "localhost:5000/app", "latest"},
		{"localhost:5000/app:2", "localhost:5000/app", "2"},
		{"ghcr.io/me/app:v1.2.3", "ghcr.io/me/app", "v1.2.3"},
	} {
		repo, tag := SplitRef(tc.in)
		if repo != tc.repo || tag != tc.tag {
			t.Errorf("SplitRef(%q) = %q, %q; want %q, %q", tc.in, repo, tag, tc.repo, tc.tag)
		}
	}
}

func TestArchive(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/abc/archive" || r.URL.Query().Get("path") != "/var/lib/data" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, "TARBYTES")
	})
	rc, err := c.Archive(context.Background(), "abc", "/var/lib/data")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "TARBYTES" {
		t.Fatalf("body = %q", b)
	}
}

func TestWait(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.44/containers/abc/wait" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"StatusCode":2}`)
	})
	if code, err := c.Wait(context.Background(), "abc"); err != nil || code != 2 {
		t.Fatalf("Wait = %d, %v; want 2", code, err)
	}
}

func TestWaitError(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"StatusCode":0,"Error":{"Message":"container gone"}}`)
	})
	if _, err := c.Wait(context.Background(), "abc"); err == nil || !strings.Contains(err.Error(), "container gone") {
		t.Fatalf("want the wait error, got %v", err)
	}
}

func TestLogs(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1.44/containers/abc/logs" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		io.WriteString(w, "bosun: backup file is unreadable\n")
	})
	if out, err := c.Logs(context.Background(), "abc"); err != nil || out != "bosun: backup file is unreadable" {
		t.Fatalf("Logs = %q, %v", out, err)
	}
}
