package docker

import (
	"context"
	"encoding/binary"
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

// A container started without a TTY gets Docker's multiplexed stream: every
// chunk carries an 8-byte header, which must not reach the reader.
func TestLogsStripsDockerFraming(t *testing.T) {
	frame := func(stream byte, text string) []byte {
		h := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(h[4:], uint32(len(text)))
		return append(h, text...)
	}
	body := append(frame(1, "starting up\n"), frame(2, "boom: no such table\n")...)
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.Write(body)
	})
	want := "starting up\nboom: no such table"
	if out, err := c.Logs(context.Background(), "abc"); err != nil || out != want {
		t.Fatalf("Logs = %q, %v, want %q", out, err, want)
	}
}

// A TTY container's stream is plain text and must pass through untouched.
func TestLogsKeepsARawStreamAsItIs(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		io.WriteString(w, "bosun: backup file is unreadable\n")
	})
	if out, err := c.Logs(context.Background(), "abc"); err != nil || out != "bosun: backup file is unreadable" {
		t.Fatalf("Logs = %q, %v", out, err)
	}
}

// The 4 KB cap can cut a frame in half. What arrived whole is still returned.
func TestLogsKeepsWhatArrivedOfACutFrame(t *testing.T) {
	h := []byte{1, 0, 0, 0, 0, 0, 0, 20}
	body := append([]byte{1, 0, 0, 0, 0, 0, 0, 5}, "first"...)
	body = append(body, h...)      // says 20 bytes
	body = append(body, "cut"...)  // only 3 arrive
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.Write(body)
	})
	if out, err := c.Logs(context.Background(), "abc"); err != nil || out != "firstcut" {
		t.Fatalf("Logs = %q, %v, want %q", out, err, "firstcut")
	}
}

func TestInfoReadsTheHostName(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+APIVersion+"/info" {
			t.Errorf("path = %s, want /info", r.URL.Path)
		}
		io.WriteString(w, `{"Name":"worker-1","OperatingSystem":"whatever"}`)
	})
	host, err := c.Info(context.Background())
	if err != nil || host != "worker-1" {
		t.Fatalf("Info = %q, %v, want worker-1", host, err)
	}
}
