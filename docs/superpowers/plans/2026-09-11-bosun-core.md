# Bosun Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Bosun's core: a no-network gate that owns the Docker socket and can only swap a container's image, plus a socket-less updater that checks registries and sends notes. This includes auto-rollback, manual rollback, the skip list, and the CLI.

**Architecture:** One Go binary, `bosun`, runs as two containers from one image. `bosun gate` talks to Docker over the unix socket through a small stdlib client. It serves a three-call JSON RPC (`list`, `update`, `events`) on `/run/bosun/gate.sock`, and it builds every Docker request itself. `bosun updater` is made by the gate. It runs rounds on a cron schedule: registry `HEAD` checks, then `update` calls, then Shoutrrr notes. The CLI runs inside the gate with `docker exec`.

**Tech Stack:** Go 1.27, stdlib `net/http` over unix sockets for Docker Engine API v1.44, `github.com/google/go-containerregistry` v0.22.1, `github.com/nicholas-fedor/shoutrrr` v0.20.0, `github.com/robfig/cron/v3` v3.0.1, distroless static image.

**Spec:** `docs/superpowers/specs/2026-09-11-bosun-design.md`

**Not in this plan (comes in plan 2, "Bosun backups"):** volume backups, the backup helper container, `rollback --with-data`, root and size warnings, and the backup columns in `status` and `rollback ls`.

## Global Constraints

- Go 1.27. `nicholas-fedor/shoutrrr` v0.20.0 needs it.
- Module path: `github.com/mehrad-meraji/bosun`.
- Only three outside libraries: `github.com/google/go-containerregistry` v0.22.1, `github.com/nicholas-fedor/shoutrrr` v0.20.0, `github.com/robfig/cron/v3` v3.0.1. No Docker SDK.
- Docker Engine API pinned to `v1.44` (Docker 25 or newer).
- Labels: `bosun.enable`, `bosun.mode`, `bosun.health-timeout`, `bosun.managed-by`.
- Names: gate container `bosun-gate`, updater container `bosun-updater`, run folder `/run/bosun`, read-only config mounts under `/etc/bosun`.
- Container removal always keeps volumes (`v=0`).
- Never log registry logins or notification URLs. Both hold secrets.
- The gate never gets network. The updater never gets `docker.sock`.
- User-facing text uses plain, short English. Every error says what to do next.

## File map

| File | Job |
|---|---|
| `main.go` | Command dispatch, env settings, `gate` and `updater` daemons. |
| `cli.go` | `status`, `check`, `rollback`, `skip`, `help`. |
| `cli_test.go` | Flag split and time formatting tests. |
| `internal/sock/sock.go` | HTTP over unix sockets: client, listen, serve, post. |
| `internal/docker/docker.go` | Minimal Docker Engine API client. |
| `internal/docker/docker_test.go` | Client tests against a fake unix-socket server. |
| `internal/recreate/recreate.go` | Builds the create body for a replacement container. |
| `internal/recreate/recreate_test.go` | Keeps user config only, copies host settings, carries volumes and networks. |
| `internal/state/state.go` | `state.json` with a file lock. |
| `internal/state/state_test.go` | Round trip, busy lock, missing file. |
| `internal/gate/gate.go` | List, update, swap, revert, health wait, rollback. |
| `internal/gate/gate_test.go` | Health verdict, trackable refs, digests. |
| `internal/gate/rpc.go` | RPC server, request checks, RPC client. |
| `internal/gate/rpc_test.go` | Request checks, refusals, fuzz. |
| `internal/gate/boot.go` | Crash recovery, updater spawn. |
| `internal/gate/boot_test.go` | Updater container settings. |
| `internal/gate/integration_test.go` | Full flow on real Docker (build tag `integration`). |
| `internal/registry/registry.go` | Registry digest checks and pull logins. |
| `internal/registry/registry_test.go` | In-memory registry tests. |
| `internal/updater/updater.go` | Rounds, schedule loop, `updater.sock`. |
| `internal/updater/notify.go` | Notify URL file and Shoutrrr sender. |
| `internal/updater/updater_test.go` | Round logic with fakes. |
| `Dockerfile`, `compose.yml`, `README.md`, `.github/workflows/ci.yml` | Ship it. |

---

### Task 1: Project setup and Docker client

**Files:**
- Create: `go.mod`, `internal/sock/sock.go`, `internal/docker/docker.go`, `internal/docker/docker_test.go`

**Interfaces:**
- Produces:
  - `sock.Client(path string) *http.Client`
  - `sock.Listen(path string) (net.Listener, error)`
  - `sock.Serve(ctx context.Context, l net.Listener, h http.Handler) error`
  - `sock.Post(ctx context.Context, c *http.Client, url string, in, out any) error`
  - `docker.New(socket string) *docker.Client`
  - `docker.Container{ID, Name, Image string; RestartCount int; State ContainerState; Config ContainerConfig; Mounts []Mount; Raw json.RawMessage}`
  - `docker.ContainerState{Running, Restarting bool; Status string; Health *Health}`, `docker.Health{Status string}`
  - `docker.ContainerConfig{Image, User string; Env []string; Labels map[string]string}`
  - `docker.Mount{Type, Name, Source, Destination string; RW bool}`
  - `docker.Image{ID string; RepoDigests []string; Config json.RawMessage}`
  - `docker.Summary{ID string; Names []string}`
  - Methods on `*docker.Client`: `List(ctx, label string, all bool) ([]Summary, error)`, `Inspect(ctx, id string) (*Container, error)`, `InspectImage(ctx, ref string) (*Image, error)`, `Pull(ctx, ref, auth string) error`, `Create(ctx, name string, body any) (string, error)`, `Start(ctx, id string) error`, `Stop(ctx, id string) error`, `Rename(ctx, id, name string) error`, `Remove(ctx, id string, force bool) error`, `Tag(ctx, image, repo, tag string) error`, `RemoveImage(ctx, ref string) error`
  - `docker.SplitRef(ref string) (repo, tag string)`, `docker.IsNotFound(err error) bool`

- [ ] **Step 1: Install Go and start the module**

Go is not on this Mac yet.

```bash
brew install go
```

```bash
go version
```

Expected: `go version go1.27` or newer.

```bash
cd /Users/mehrad/Projects/bosun && go mod init github.com/mehrad-meraji/bosun && go mod edit -go=1.27
```

- [ ] **Step 2: Write `internal/sock/sock.go`**

```go
// Package sock has small helpers for HTTP over unix sockets. The gate, the
// updater and the CLI all talk this way. There are no network ports.
package sock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client returns an HTTP client that dials the unix socket at path.
func Client(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// Listen replaces any stale socket at path and makes it owner-only.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve runs h on l until ctx ends.
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Post sends in as JSON and decodes a 200 reply into out. Any other status
// becomes an error holding the reply text.
func Post(ctx context.Context, c *http.Client, url string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Write the failing Docker client tests**

`internal/docker/docker_test.go`:

```go
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
```

- [ ] **Step 4: Run the tests to see them fail**

Run: `go test ./internal/docker/`
Expected: FAIL. `New`, `Pull` and the others are not defined.

- [ ] **Step 5: Write `internal/docker/docker.go`**

```go
// Package docker is a small Docker Engine API client.
// ponytail: stdlib over the unix socket, not the Docker SDK. Bosun needs a
// dozen calls, and raw JSON lets recreate copy HostConfig without dropping
// fields a typed SDK does not know about.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mehrad-meraji/bosun/internal/sock"
)

// APIVersion is pinned. v1.44 is Docker 25, the first that accepts several
// networks when a container is created.
const APIVersion = "v1.44"

type Client struct{ http *http.Client }

func New(socket string) *Client { return &Client{http: sock.Client(socket)} }

// Error is a Docker reply with a status of 300 or more (except 304).
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %d: %s", e.Status, e.Message) }

// IsNotFound reports whether err is a Docker 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

type Container struct {
	ID           string `json:"Id"`
	Name         string
	Image        string // image ID
	RestartCount int
	State        ContainerState
	Config       ContainerConfig
	Mounts       []Mount
	Raw          json.RawMessage `json:"-"` // the full inspect reply
}

type ContainerState struct {
	Running    bool
	Restarting bool
	Status     string
	Health     *Health
}

type Health struct{ Status string }

type ContainerConfig struct {
	Image  string
	User   string
	Env    []string
	Labels map[string]string
}

type Mount struct {
	Type        string
	Name        string
	Source      string
	Destination string
	RW          bool
}

type Image struct {
	ID          string `json:"Id"`
	RepoDigests []string
	Config      json.RawMessage
}

type Summary struct {
	ID    string `json:"Id"`
	Names []string
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, hdr http.Header) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := "http://docker/" + APIVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		var m struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return nil, &Error{Status: resp.StatusCode, Message: m.Message}
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, q, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// List returns containers with the given label ("key=value"). all includes
// stopped ones.
func (c *Client) List(ctx context.Context, label string, all bool) ([]Summary, error) {
	f, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	q := url.Values{"filters": {string(f)}}
	if all {
		q.Set("all", "1")
	}
	var out []Summary
	if err := c.call(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Inspect returns a container by name or ID. Raw keeps the full reply.
func (c *Client) Inspect(ctx context.Context, id string) (*Container, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &raw); err != nil {
		return nil, err
	}
	var ct Container
	if err := json.Unmarshal(raw, &ct); err != nil {
		return nil, err
	}
	ct.Raw = raw
	return &ct, nil
}

// InspectImage returns an image by ID or ref.
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	var img Image
	if err := c.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// Pull pulls ref. auth is the X-Registry-Auth value, or "" for anonymous.
// Docker reports pull errors inside a 200 stream, so the stream is read to
// the end.
func (c *Client) Pull(ctx context.Context, ref, auth string) error {
	repo, tag := SplitRef(ref)
	hdr := http.Header{}
	if auth != "" {
		hdr.Set("X-Registry-Auth", auth)
	}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {repo}, "tag": {tag}}, nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error string `json:"error"`
		}
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if m.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, m.Error)
		}
	}
}

// Create makes a container and returns its ID.
func (c *Client) Create(ctx context.Context, name string, body any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", nil, nil, nil)
}

func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/rename", url.Values{"name": {name}}, nil, nil)
}

// Remove deletes a container. Its volumes are always kept.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	q := url.Values{"v": {"0"}}
	if force {
		q.Set("force", "1")
	}
	return c.call(ctx, http.MethodDelete, "/containers/"+id, q, nil, nil)
}

// Tag adds repo:tag to an image.
func (c *Client) Tag(ctx context.Context, image, repo, tag string) error {
	return c.call(ctx, http.MethodPost, "/images/"+image+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
}

// RemoveImage removes a tag, and the image if nothing else uses it.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	return c.call(ctx, http.MethodDelete, "/images/"+ref, nil, nil, nil)
}

// SplitRef splits "host:5000/app:1.2" into ("host:5000/app", "1.2").
// No tag means "latest".
func SplitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `go test ./internal/... && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod internal/sock internal/docker
git commit -m "Add unix socket helpers and a small Docker client"
```

---

### Task 2: Recreate builder

This is the security core. It builds the new container from the old one, and only the image changes.

**Files:**
- Create: `internal/recreate/recreate.go`, `internal/recreate/recreate_test.go`

**Interfaces:**
- Produces: `recreate.Build(old, imgConfig json.RawMessage, newRef string) (json.RawMessage, error)`. `old` is the full container inspect JSON (`docker.Container.Raw`). `imgConfig` is `docker.Image.Config` of the old container's image.

- [ ] **Step 1: Write the failing tests**

`internal/recreate/recreate_test.go`:

```go
package recreate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const oldJSON = `{
 "Id": "0123456789abcdef0123",
 "Config": {
   "Hostname": "0123456789ab",
   "Image": "nginx:1.27",
   "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27.1", "FOO=bar"],
   "Cmd": ["nginx", "-g", "daemon off;"],
   "Entrypoint": ["/docker-entrypoint.sh"],
   "Labels": {"maintainer": "NGINX", "bosun.enable": "true", "com.docker.compose.service": "web"},
   "ExposedPorts": {"80/tcp": {}}
 },
 "HostConfig": {
   "Binds": ["/srv/site:/usr/share/nginx/html:ro"],
   "NetworkMode": "web_default",
   "Privileged": false,
   "CapAdd": null,
   "RestartPolicy": {"Name": "unless-stopped"}
 },
 "NetworkSettings": {"Networks": {
   "web_default": {"Aliases": ["web", "0123456789ab"], "IPAMConfig": null, "NetworkID": "n1", "EndpointID": "e1", "IPAddress": "172.18.0.2"},
   "proxy": {"Aliases": ["web"], "IPAMConfig": {"IPv4Address": "10.0.0.5"}}
 }},
 "Mounts": [
   {"Type": "bind", "Source": "/srv/site", "Destination": "/usr/share/nginx/html"},
   {"Type": "volume", "Name": "3f9a", "Destination": "/var/cache/nginx"}
 ]
}`

const imgJSON = `{
 "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27.1"],
 "Cmd": ["nginx", "-g", "daemon off;"],
 "Entrypoint": ["/docker-entrypoint.sh"],
 "Labels": {"maintainer": "NGINX"},
 "ExposedPorts": {"80/tcp": {}}
}`

func build(t *testing.T, old string) map[string]any {
	t.Helper()
	b, err := Build(json.RawMessage(old), json.RawMessage(imgJSON), "nginx:1.28")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildKeepsOnlyUserConfig(t *testing.T) {
	m := build(t, oldJSON)
	if m["Image"] != "nginx:1.28" {
		t.Errorf("Image = %v", m["Image"])
	}
	if !reflect.DeepEqual(m["Env"], []any{"FOO=bar"}) {
		t.Errorf("Env = %v, want only the user's FOO=bar", m["Env"])
	}
	want := map[string]any{"bosun.enable": "true", "com.docker.compose.service": "web"}
	if !reflect.DeepEqual(m["Labels"], want) {
		t.Errorf("Labels = %v, want %v", m["Labels"], want)
	}
	for _, k := range []string{"Cmd", "Entrypoint", "ExposedPorts", "Hostname"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s was copied from the old image; the new image must supply it", k)
		}
	}
}

func TestBuildKeepsUserCmd(t *testing.T) {
	old := strings.Replace(oldJSON, `"Cmd": ["nginx", "-g", "daemon off;"]`, `"Cmd": ["nginx", "-T"]`, 1)
	m := build(t, old)
	if !reflect.DeepEqual(m["Cmd"], []any{"nginx", "-T"}) {
		t.Errorf("Cmd = %v, want the user's override", m["Cmd"])
	}
}

func TestBuildKeepsCustomHostname(t *testing.T) {
	old := strings.Replace(oldJSON, `"Hostname": "0123456789ab"`, `"Hostname": "web01"`, 1)
	if m := build(t, old); m["Hostname"] != "web01" {
		t.Errorf("Hostname = %v, want web01", m["Hostname"])
	}
}

// The new container may gain nothing on the host that the old one did not
// have. HostConfig is copied, and the only addition allowed is the old
// container's own anonymous volumes.
func TestBuildHostConfigOnlyGainsOldVolumes(t *testing.T) {
	var old struct{ HostConfig map[string]any }
	json.Unmarshal([]byte(oldJSON), &old)
	hc := build(t, oldJSON)["HostConfig"].(map[string]any)

	mounts := hc["Mounts"].([]any)
	delete(hc, "Mounts")
	if !reflect.DeepEqual(hc, old.HostConfig) {
		t.Errorf("HostConfig changed:\n got  %v\n want %v", hc, old.HostConfig)
	}
	want := []any{map[string]any{"Type": "volume", "Source": "3f9a", "Target": "/var/cache/nginx"}}
	if !reflect.DeepEqual(mounts, want) {
		t.Errorf("Mounts = %v, want only the anonymous volume %v", mounts, want)
	}
}

func TestBuildNamedVolumesAreNotAddedTwice(t *testing.T) {
	old := strings.Replace(oldJSON, `"Binds": ["/srv/site:/usr/share/nginx/html:ro"]`,
		`"Binds": ["/srv/site:/usr/share/nginx/html:ro", "3f9a:/var/cache/nginx"]`, 1)
	hc := build(t, old)["HostConfig"].(map[string]any)
	if _, ok := hc["Mounts"]; ok {
		t.Errorf("volume already in Binds was added again: %v", hc["Mounts"])
	}
}

func TestBuildNetworks(t *testing.T) {
	m := build(t, oldJSON)
	eps := m["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	web := eps["web_default"].(map[string]any)
	if !reflect.DeepEqual(web["Aliases"], []any{"web"}) {
		t.Errorf("web_default aliases = %v, want [web] without the old ID", web["Aliases"])
	}
	for _, k := range []string{"NetworkID", "EndpointID", "IPAddress"} {
		if _, ok := web[k]; ok {
			t.Errorf("%s copied; Docker must assign it", k)
		}
	}
	proxy := eps["proxy"].(map[string]any)
	if !reflect.DeepEqual(proxy["IPAMConfig"], map[string]any{"IPv4Address": "10.0.0.5"}) {
		t.Errorf("proxy lost its static IP: %v", proxy)
	}
}

func TestBuildHostNetworkHasNoEndpoints(t *testing.T) {
	old := strings.Replace(oldJSON, `"NetworkMode": "web_default"`, `"NetworkMode": "host"`, 1)
	if _, ok := build(t, old)["NetworkingConfig"]; ok {
		t.Error("host network mode must not get NetworkingConfig")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/recreate/`
Expected: FAIL. `Build` is not defined.

- [ ] **Step 3: Write `internal/recreate/recreate.go`**

```go
// Package recreate builds the Docker create request for a container's
// replacement. Only the image changes. Everything else comes from the old
// container. This is where "the gate can only swap images" is made true.
package recreate

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

type mount struct{ Type, Name, Destination string }

type oldContainer struct {
	ID              string `json:"Id"`
	Config          map[string]json.RawMessage
	HostConfig      map[string]json.RawMessage
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage
	}
	Mounts []mount
}

// Build returns the create body for a copy of old that runs newRef.
// old is the full inspect JSON of the old container. imgConfig is the Config
// of the old container's image. Values that only repeat image defaults are
// left out, so the new image can supply its own.
func Build(old, imgConfig json.RawMessage, newRef string) (json.RawMessage, error) {
	var c oldContainer
	if err := json.Unmarshal(old, &c); err != nil {
		return nil, err
	}
	var img map[string]json.RawMessage
	if err := unmarshalOpt(imgConfig, &img); err != nil {
		return nil, err
	}
	body := map[string]any{}
	for k, v := range c.Config {
		keep, err := userValue(k, v, img[k], c.ID)
		if err != nil {
			return nil, err
		}
		if keep != nil {
			body[k] = keep
		}
	}
	body["Image"] = newRef
	hc, err := withAnonVolumes(c.HostConfig, c.Mounts)
	if err != nil {
		return nil, err
	}
	body["HostConfig"] = hc
	if nc := networking(c.HostConfig, c.NetworkSettings.Networks, c.ID); nc != nil {
		body["NetworkingConfig"] = nc
	}
	return json.Marshal(body)
}

// userValue returns the part of a Config value the user set, or nil when the
// value only repeats the image default.
func userValue(key string, v, img json.RawMessage, id string) (json.RawMessage, error) {
	switch key {
	case "Image":
		return nil, nil
	case "Hostname":
		// Docker sets the hostname to the short container ID by default.
		// Copying it would name the new container after the old one.
		var h string
		if err := json.Unmarshal(v, &h); err != nil {
			return nil, err
		}
		if h == "" || (len(id) >= 12 && h == id[:12]) {
			return nil, nil
		}
		return v, nil
	case "Env":
		var got, def []string
		if err := unmarshalOpt(v, &got); err != nil {
			return nil, err
		}
		if err := unmarshalOpt(img, &def); err != nil {
			return nil, err
		}
		got = slices.DeleteFunc(got, func(e string) bool { return slices.Contains(def, e) })
		if len(got) == 0 {
			return nil, nil
		}
		return json.Marshal(got)
	case "Labels", "ExposedPorts", "Volumes":
		var got, def map[string]json.RawMessage
		if err := unmarshalOpt(v, &got); err != nil {
			return nil, err
		}
		if err := unmarshalOpt(img, &def); err != nil {
			return nil, err
		}
		maps.DeleteFunc(got, func(k string, val json.RawMessage) bool {
			d, ok := def[k]
			return ok && same(val, d)
		})
		if len(got) == 0 {
			return nil, nil
		}
		return json.Marshal(got)
	}
	if same(v, img) {
		return nil, nil
	}
	return v, nil
}

// withAnonVolumes adds the old container's anonymous volumes (from the image's
// VOLUME lines) to HostConfig.Mounts by name, so their data carries over.
// Named volumes and binds are already in HostConfig.
func withAnonVolumes(hc map[string]json.RawMessage, mounts []mount) (map[string]json.RawMessage, error) {
	covered := map[string]bool{}
	var binds []string
	if err := unmarshalOpt(hc["Binds"], &binds); err != nil {
		return nil, err
	}
	for _, b := range binds {
		if p := strings.Split(b, ":"); len(p) >= 2 {
			covered[p[1]] = true
		}
	}
	var ms []map[string]any
	if err := unmarshalOpt(hc["Mounts"], &ms); err != nil {
		return nil, err
	}
	for _, m := range ms {
		if t, ok := m["Target"].(string); ok {
			covered[t] = true
		}
	}
	n := len(ms)
	for _, m := range mounts {
		if m.Type == "volume" && m.Name != "" && !covered[m.Destination] {
			ms = append(ms, map[string]any{"Type": "volume", "Source": m.Name, "Target": m.Destination})
		}
	}
	if len(ms) == n {
		return hc, nil
	}
	b, err := json.Marshal(ms)
	if err != nil {
		return nil, err
	}
	out := maps.Clone(hc)
	out["Mounts"] = b
	return out, nil
}

// networking reconnects the new container to every network the old one was
// on, with its aliases and static IPs. Docker assigns the rest.
func networking(hc map[string]json.RawMessage, nets map[string]map[string]json.RawMessage, id string) map[string]any {
	var mode string
	_ = unmarshalOpt(hc["NetworkMode"], &mode)
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") || len(nets) == 0 {
		return nil
	}
	eps := map[string]any{}
	for name, ep := range nets {
		e := map[string]any{}
		for _, k := range []string{"IPAMConfig", "Links", "DriverOpts"} {
			if v, ok := ep[k]; ok && string(v) != "null" {
				e[k] = v
			}
		}
		var aliases []string
		_ = unmarshalOpt(ep["Aliases"], &aliases)
		aliases = slices.DeleteFunc(aliases, func(a string) bool { return len(id) >= 12 && a == id[:12] })
		if len(aliases) > 0 {
			e["Aliases"] = aliases
		}
		eps[name] = e
	}
	return map[string]any{"EndpointsConfig": eps}
}

func unmarshalOpt(b json.RawMessage, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func same(a, b json.RawMessage) bool {
	if len(a) == 0 {
		a = json.RawMessage("null")
	}
	if len(b) == 0 {
		b = json.RawMessage("null")
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/recreate/ -v`
Expected: PASS, 7 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/recreate
git commit -m "Add recreate builder: only the image changes"
```

---

### Task 3: State file

**Files:**
- Create: `internal/state/state.go`, `internal/state/state_test.go`

**Interfaces:**
- Produces:
  - `state.State{Containers map[string]*Entry; Pending []Pending; Events []Event}`
  - `state.Entry{Prev string; UpdatedAt time.Time; Downtime time.Duration; Skip []string}`, method `(*Entry).AddSkip(digest string)`
  - `state.Pending{Name, OldID, TmpName string}` (comparable)
  - `state.Event{Time time.Time; Kind, Name, Message string}`
  - `state.Open(dir string, wait bool) (*File, *State, error)`, `(*File).Save(*State) error`, `(*File).Close() error`
  - `state.Read(dir string) (*State, error)`
  - `(*State).Entry(name string) *Entry`, `(*State).AddEvent(kind, name, msg string)`
  - `state.ErrBusy`

- [ ] **Step 1: Write the failing tests**

`internal/state/state_test.go`:

```go
package state

import (
	"errors"
	"testing"
	"time"
)

func TestSaveAndRead(t *testing.T) {
	dir := t.TempDir()
	f, s, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Entry("web")
	e.Prev = "bosun/prev/web:abc"
	e.Downtime = 2 * time.Second
	e.AddSkip("sha256:1")
	e.AddSkip("sha256:1")
	s.Pending = append(s.Pending, Pending{Name: "web", OldID: "id1", TmpName: "web-bosun-aa"})
	s.AddEvent("recovered", "web", "old version restored")
	if err := f.Save(s); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := got.Containers["web"]
	if g.Prev != e.Prev || g.Downtime != e.Downtime || len(g.Skip) != 1 {
		t.Errorf("entry = %+v, want %+v with one skip", g, e)
	}
	if len(got.Pending) != 1 || got.Pending[0].TmpName != "web-bosun-aa" {
		t.Errorf("pending = %+v", got.Pending)
	}
	if len(got.Events) != 1 || got.Events[0].Message != "old version restored" {
		t.Errorf("events = %+v", got.Events)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	s, err := Read(t.TempDir())
	if err != nil || s.Containers == nil || len(s.Containers) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestOpenNoWaitIsBusy(t *testing.T) {
	dir := t.TempDir()
	f, _, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := Open(dir, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/state/`
Expected: FAIL. `Open` is not defined.

- [ ] **Step 3: Write `internal/state/state.go`**

```go
// Package state is the gate's small state file, /run/bosun/state.json.
// Docker labels cannot change after a container is made, so the skip list,
// rollback records and in-progress swaps live here. Only the gate writes it.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

type State struct {
	Containers map[string]*Entry `json:"containers"`
	Pending    []Pending         `json:"pending,omitempty"`
	Events     []Event           `json:"events,omitempty"`
}

type Entry struct {
	Prev      string        `json:"prev,omitempty"` // local tag of the kept old image
	UpdatedAt time.Time     `json:"updated_at,omitzero"`
	Downtime  time.Duration `json:"downtime,omitempty"`
	Skip      []string      `json:"skip,omitempty"` // digests never to update to
}

// Pending is a swap in progress. If the gate dies mid-swap, Recover uses it.
type Pending struct {
	Name    string `json:"name"`
	OldID   string `json:"old_id"`
	TmpName string `json:"tmp_name"`
}

// Event waits here until the updater collects it and sends it as a note.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Message string    `json:"message"`
}

var ErrBusy = errors.New("bosun is busy with an update, try again later")

type File struct {
	dir  string
	lock *os.File
}

func path(dir string) string { return filepath.Join(dir, "state.json") }

// Open takes the state lock and loads the state. With wait false it returns
// ErrBusy at once if another process holds the lock.
func Open(dir string, wait bool) (*File, *State, error) {
	lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(lf.Fd()), how); err != nil {
		lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil, ErrBusy
		}
		return nil, nil, err
	}
	s, err := Read(dir)
	if err != nil {
		lf.Close()
		return nil, nil, err
	}
	return &File{dir: dir, lock: lf}, s, nil
}

// Read loads the state without the lock. Use it for display only.
func Read(dir string) (*State, error) {
	s := &State{}
	b, err := os.ReadFile(path(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, s); err != nil {
			return nil, fmt.Errorf("read %s: %w", path(dir), err)
		}
	}
	if s.Containers == nil {
		s.Containers = map[string]*Entry{}
	}
	return s, nil
}

// Save writes the state in one atomic step.
func (f *File) Save(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path(f.dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(f.dir))
}

// Close releases the lock.
func (f *File) Close() error { return f.lock.Close() }

// Entry returns the entry for name, making it if needed.
func (s *State) Entry(name string) *Entry {
	e := s.Containers[name]
	if e == nil {
		e = &Entry{}
		s.Containers[name] = e
	}
	return e
}

func (s *State) AddEvent(kind, name, msg string) {
	s.Events = append(s.Events, Event{Time: time.Now().UTC(), Kind: kind, Name: name, Message: msg})
}

func (e *Entry) AddSkip(digest string) {
	if !slices.Contains(e.Skip, digest) {
		e.Skip = append(e.Skip, digest)
	}
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/state/ -v`
Expected: PASS, 3 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/state
git commit -m "Add gate state file with a lock"
```

---

### Task 4: Gate core (list, update, swap, rollback)

**Files:**
- Create: `internal/gate/gate.go`, `internal/gate/gate_test.go`

**Interfaces:**
- Consumes: `docker.Client` and its types (Task 1), `recreate.Build` (Task 2), `state.*` (Task 3).
- Produces:
  - Constants `gate.LabelEnable`, `LabelMode`, `LabelTimeout`, `LabelManaged`, `UpdaterName`, `StatusDone = "done"`, `StatusReverted = "reverted"`
  - `gate.Gate{D *docker.Client; Dir string; SelfID string; Poll time.Duration}`
  - `gate.Watched{Name, Ref string; Digests []string; Mode string; Skip []string}` (JSON: `name`, `ref`, `digests`, `mode`, `skip`)
  - `gate.Result{Status string; Downtime time.Duration; Message string}` (JSON: `status`, `downtime`, `message`)
  - `gate.RefusedError{Msg string}`
  - `(*Gate).List(ctx) ([]Watched, error)`
  - `(*Gate).Update(ctx, name, digest, auth string) (Result, error)`
  - `(*Gate).Rollback(ctx, name string) (Result, error)`

The full swap flow runs against real Docker in Task 10. Here we unit test the pure parts.

- [ ] **Step 1: Write the failing tests**

`internal/gate/gate_test.go`:

```go
package gate

import (
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
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/gate/`
Expected: FAIL. `verdict` is not defined.

- [ ] **Step 3: Write `internal/gate/gate.go`**

```go
// Package gate is the only part of Bosun that talks to Docker. It has no
// network. It offers a few fixed actions and builds every Docker request
// itself, so a caller can never add mounts, privileges or devices.
package gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/recreate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const (
	LabelEnable  = "bosun.enable"
	LabelMode    = "bosun.mode"
	LabelTimeout = "bosun.health-timeout"
	LabelManaged = "bosun.managed-by"
	UpdaterName  = "bosun-updater"

	StatusDone     = "done"
	StatusReverted = "reverted"
)

type Gate struct {
	D      *docker.Client
	Dir    string        // shared run folder, /run/bosun
	SelfID string        // the gate's own container ID (or its prefix); never touched
	Poll   time.Duration // health poll interval; 0 means 1s
}

// Watched is a container Bosun looks after.
type Watched struct {
	Name    string   `json:"name"`
	Ref     string   `json:"ref"`
	Digests []string `json:"digests"`
	Mode    string   `json:"mode"` // "update" or "notify"
	Skip    []string `json:"skip"`
}

type Result struct {
	Status   string        `json:"status"` // StatusDone or StatusReverted
	Downtime time.Duration `json:"downtime"`
	Message  string        `json:"message"`
}

// RefusedError is a request the gate will not do. It means a bug or an attack.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return "refused: " + e.Msg }

func refuse(format string, a ...any) error { return &RefusedError{Msg: fmt.Sprintf(format, a...)} }

// List returns the running containers with bosun.enable=true that follow a tag.
func (g *Gate) List(ctx context.Context) ([]Watched, error) {
	sums, err := g.D.List(ctx, LabelEnable+"=true", false)
	if err != nil {
		return nil, err
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		return nil, err
	}
	out := []Watched{}
	for _, s := range sums {
		c, err := g.D.Inspect(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		ref, ok := trackable(c)
		if !ok || g.isSelf(c) {
			continue
		}
		img, err := g.D.InspectImage(ctx, c.Image)
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(c.Name, "/")
		mode := c.Config.Labels[LabelMode]
		if mode != "notify" {
			mode = "update"
		}
		out = append(out, Watched{Name: name, Ref: ref, Digests: digestsOf(img), Mode: mode, Skip: st.Entry(name).Skip})
	}
	return out, nil
}

// Update pulls the container's own tag, checks it is digest, and swaps the
// container onto it. The image ref always comes from the container, never
// from the caller.
func (g *Gate) Update(ctx context.Context, name, digest, auth string) (Result, error) {
	c, ref, err := g.watched(ctx, name)
	if err != nil {
		return Result{}, err
	}
	if c.Config.Labels[LabelMode] == "notify" {
		return Result{}, refuse("%s is notify-only", name)
	}
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	e := st.Entry(name)
	if slices.Contains(e.Skip, digest) {
		return Result{}, refuse("%s: %s is on the skip list", name, digest)
	}
	if err := g.D.Pull(ctx, ref, auth); err != nil {
		return Result{}, err
	}
	img, err := g.D.InspectImage(ctx, ref)
	if err != nil {
		return Result{}, err
	}
	if !slices.Contains(digestsOf(img), digest) {
		return Result{}, fmt.Errorf("%s: pulled %s but it is not %s (the tag moved?); skipped this round", name, ref, digest)
	}
	// From here on, finish even if the caller hangs up. A half-done swap is worse.
	ctx = context.WithoutCancel(ctx)
	res, err := g.swap(ctx, f, st, c, ref)
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		e.AddSkip(digest)
		return res, f.Save(st)
	}
	prev, err := g.keep(ctx, name, c.Image, e.Prev)
	if err != nil {
		log.Printf("%s: could not keep the old image for rollback: %v", name, err)
	}
	e.Prev, e.UpdatedAt, e.Downtime = prev, time.Now().UTC(), res.Downtime
	return res, f.Save(st)
}

// Rollback puts back the version kept by the last update.
func (g *Gate) Rollback(ctx context.Context, name string) (Result, error) {
	f, st, err := state.Open(g.Dir, false)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	e := st.Entry(name)
	if e.Prev == "" {
		return Result{}, fmt.Errorf("no old version kept for %s. Run `bosun rollback ls` to see what you can roll back", name)
	}
	c, ref, err := g.watched(ctx, name)
	if err != nil {
		return Result{}, err
	}
	cur, err := g.D.InspectImage(ctx, c.Image)
	if err != nil {
		return Result{}, err
	}
	repo, tag := docker.SplitRef(ref)
	// Point the tag back at the old image, so the container keeps a readable ref.
	if err := g.D.Tag(ctx, e.Prev, repo, tag); err != nil {
		return Result{}, fmt.Errorf("retag %s: %w", e.Prev, err)
	}
	ctx = context.WithoutCancel(ctx)
	res, err := g.swap(ctx, f, st, c, ref)
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		_ = g.D.Tag(ctx, c.Image, repo, tag)
		res.Message = fmt.Sprintf("%s: the old version did not come up healthy; the current version is kept", name)
		return res, nil
	}
	for _, d := range digestsOf(cur) {
		e.AddSkip(d)
	}
	_ = g.D.RemoveImage(ctx, e.Prev)
	e.Prev, e.UpdatedAt = "", time.Now().UTC()
	res.Message = fmt.Sprintf("%s: rolled back (down %s). The newer version is on the skip list; `bosun skip clear %s` allows it again",
		name, res.Downtime.Round(100*time.Millisecond), name)
	return res, f.Save(st)
}

// swap replaces old with a new container running ref, then waits for it to
// be healthy. If anything fails after the old one stops, it puts the old one
// back and returns StatusReverted.
func (g *Gate) swap(ctx context.Context, f *state.File, st *state.State, old *docker.Container, ref string) (Result, error) {
	name := strings.TrimPrefix(old.Name, "/")
	img, err := g.D.InspectImage(ctx, old.Image)
	if err != nil {
		return Result{}, err
	}
	body, err := recreate.Build(old.Raw, img.Config, ref)
	if err != nil {
		return Result{}, fmt.Errorf("%s: build the new container: %w", name, err)
	}

	p := state.Pending{Name: name, OldID: old.ID, TmpName: name + "-bosun-" + randHex()}
	st.Pending = append(st.Pending, p)
	if err := f.Save(st); err != nil {
		return Result{}, err
	}
	defer func() {
		st.Pending = slices.DeleteFunc(st.Pending, func(q state.Pending) bool { return q == p })
		if err := f.Save(st); err != nil {
			log.Printf("save state: %v", err)
		}
	}()

	t0 := time.Now()
	if err := g.D.Stop(ctx, old.ID); err != nil {
		return Result{}, fmt.Errorf("%s: stop: %w", name, err)
	}
	if err := g.D.Rename(ctx, old.ID, p.TmpName); err != nil {
		_ = g.D.Start(ctx, old.ID)
		return Result{}, fmt.Errorf("%s: rename: %w", name, err)
	}
	newID, err := g.D.Create(ctx, name, body)
	if err == nil {
		err = g.D.Start(ctx, newID)
	}
	down := time.Since(t0)
	if err == nil {
		err = g.waitHealthy(ctx, newID, timeoutOf(old))
	}
	if err != nil {
		if rerr := g.revert(ctx, old.ID, newID, name); rerr != nil {
			return Result{}, fmt.Errorf("%s: new version failed (%v) and putting the old one back failed: %w", name, err, rerr)
		}
		return Result{Status: StatusReverted, Downtime: time.Since(t0),
			Message: fmt.Sprintf("%s: new version failed (%v); the old version is back", name, err)}, nil
	}
	if err := g.D.Remove(ctx, old.ID, true); err != nil {
		log.Printf("%s: remove old container %s: %v", name, p.TmpName, err)
	}
	return Result{Status: StatusDone, Downtime: down,
		Message: fmt.Sprintf("%s: now running %s (down %s)", name, ref, down.Round(100*time.Millisecond))}, nil
}

func (g *Gate) revert(ctx context.Context, oldID, newID, name string) error {
	if newID != "" {
		if err := g.D.Remove(ctx, newID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	if err := g.D.Rename(ctx, oldID, name); err != nil {
		return err
	}
	return g.D.Start(ctx, oldID)
}

func (g *Gate) waitHealthy(ctx context.Context, id string, timeout time.Duration) error {
	poll := g.Poll
	if poll == 0 {
		poll = time.Second
	}
	t0 := time.Now()
	for {
		c, err := g.D.Inspect(ctx, id)
		if err != nil {
			return err
		}
		if done, err := verdict(c, time.Since(t0), timeout); done {
			return err
		}
		time.Sleep(poll)
	}
}

// verdict judges one look at the new container. done=false means keep
// waiting. With a HEALTHCHECK it must report healthy in time. Without one it
// must keep running, with no restarts, for the whole timeout.
func verdict(c *docker.Container, elapsed, timeout time.Duration) (done bool, err error) {
	if !c.State.Running || c.State.Restarting || c.RestartCount > 0 {
		return true, fmt.Errorf("container stopped (%s)", c.State.Status)
	}
	if h := c.State.Health; h != nil {
		switch h.Status {
		case "healthy":
			return true, nil
		case "unhealthy":
			return true, errors.New("healthcheck says unhealthy")
		}
		if elapsed >= timeout {
			return true, fmt.Errorf("not healthy after %s", timeout)
		}
		return false, nil
	}
	return elapsed >= timeout, nil
}

// keep tags the old image as bosun/prev/<name>:<id> so `docker image prune`
// leaves it for rollback, and drops the tag of the version kept before it.
func (g *Gate) keep(ctx context.Context, name, imageID, before string) (string, error) {
	repo := "bosun/prev/" + strings.ToLower(name)
	tag := strings.TrimPrefix(imageID, "sha256:")
	if len(tag) > 12 {
		tag = tag[:12]
	}
	if err := g.D.Tag(ctx, imageID, repo, tag); err != nil {
		return "", err
	}
	if before != "" && before != repo+":"+tag {
		_ = g.D.RemoveImage(ctx, before)
	}
	return repo + ":" + tag, nil
}

// watched loads a container the gate may change, or refuses.
func (g *Gate) watched(ctx context.Context, name string) (*docker.Container, string, error) {
	c, err := g.D.Inspect(ctx, name)
	if docker.IsNotFound(err) {
		return nil, "", refuse("no container named %q", name)
	}
	if err != nil {
		return nil, "", err
	}
	if c.Config.Labels[LabelEnable] != "true" {
		return nil, "", refuse("%s does not have %s=true", name, LabelEnable)
	}
	if g.isSelf(c) {
		return nil, "", refuse("%s is part of bosun", name)
	}
	ref, ok := trackable(c)
	if !ok {
		return nil, "", refuse("%s runs %q, which is pinned or not a tag", name, c.Config.Image)
	}
	return c, ref, nil
}

func (g *Gate) isSelf(c *docker.Container) bool {
	return (g.SelfID != "" && strings.HasPrefix(c.ID, g.SelfID)) || c.Config.Labels[LabelManaged] != ""
}

// trackable returns the tag a container follows. Images run by ID or pinned
// by digest are never updated.
func trackable(c *docker.Container) (string, bool) {
	ref := c.Config.Image
	if ref == "" || strings.HasPrefix(ref, "sha256:") || strings.Contains(ref, "@") {
		return "", false
	}
	return ref, true
}

func digestsOf(img *docker.Image) []string {
	var out []string
	for _, rd := range img.RepoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok {
			out = append(out, d)
		}
	}
	return out
}

func timeoutOf(c *docker.Container) time.Duration {
	if d, err := time.ParseDuration(c.Config.Labels[LabelTimeout]); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

func randHex() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/gate/ -v && go vet ./...`
Expected: PASS, 4 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/gate
git commit -m "Add gate update, swap with auto-rollback, and manual rollback"
```

---

### Task 5: Gate RPC

**Files:**
- Create: `internal/gate/rpc.go`, `internal/gate/rpc_test.go`

**Interfaces:**
- Consumes: `sock.Serve`, `sock.Post`, `sock.Client` (Task 1), `Gate.List`, `Gate.Update` (Task 4), `state.Open`, `state.Event` (Task 3).
- Produces:
  - `gate.UpdateRequest{Name, Digest, Auth string}` (JSON `name`, `digest`, `auth`), `(UpdateRequest).Validate() error`
  - `(*Gate).Serve(ctx context.Context, l net.Listener) error`
  - `gate.Dial(socket string) *gate.Client`
  - `(*gate.Client).List(ctx) ([]Watched, error)`, `.Update(ctx, name, digest, auth string) (Result, error)`, `.Events(ctx) ([]state.Event, error)`

- [ ] **Step 1: Write the failing tests**

`internal/gate/rpc_test.go`:

```go
package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/sock"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		req UpdateRequest
		ok  bool
	}{
		{UpdateRequest{Name: "web", Digest: goodDigest}, true},
		{UpdateRequest{Name: "my_app.v2-x", Digest: goodDigest}, true},
		{UpdateRequest{Name: "../etc", Digest: goodDigest}, false},
		{UpdateRequest{Name: "a b", Digest: goodDigest}, false},
		{UpdateRequest{Name: "", Digest: goodDigest}, false},
		{UpdateRequest{Name: "web", Digest: "sha256:XYZ"}, false},
		{UpdateRequest{Name: "web", Digest: "md5:" + strings.Repeat("a", 64)}, false},
	} {
		err := tc.req.Validate()
		var ref *RefusedError
		if tc.ok != (err == nil) || (err != nil && !errors.As(err, &ref)) {
			t.Errorf("Validate(%+v) = %v, want ok=%v and a RefusedError on failure", tc.req, err, tc.ok)
		}
	}
}

func TestDecodeRefusesUnknownFields(t *testing.T) {
	var r UpdateRequest
	err := decode(strings.NewReader(`{"name":"web","digest":"`+goodDigest+`","auth":"","image":"evil:latest"}`), &r)
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("want a refusal for an unknown field, got %v", err)
	}
}

func TestDecodeRefusesTwoObjects(t *testing.T) {
	var r UpdateRequest
	if err := decode(strings.NewReader(`{"name":"a"} {"name":"b"}`), &r); err == nil {
		t.Fatal("want a refusal for two objects")
	}
}

// A bad request is refused before Docker is touched. Gate.D is nil here, so
// any Docker call would panic.
func TestServeRefusesBadUpdate(t *testing.T) {
	dir, err := os.MkdirTemp("", "bg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "g.sock")
	l, err := sock.Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Gate{Dir: dir}).Serve(ctx, l)

	_, err = Dial(p).Update(ctx, "web", "sha256:nope", "")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want refused, got %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

func FuzzUpdateRequest(f *testing.F) {
	f.Add([]byte(`{"name":"web","digest":"` + goodDigest + `","auth":"eyJ1c2VybmFtZSI6InUifQ=="}`))
	f.Add([]byte(`{"name":"web","digest":null}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r UpdateRequest
		if decode(bytes.NewReader(b), &r) != nil || r.Validate() != nil {
			return
		}
		if !nameRE.MatchString(r.Name) || !digestRE.MatchString(r.Digest) {
			t.Fatalf("accepted a bad request: %+v", r)
		}
	})
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/gate/`
Expected: FAIL. `UpdateRequest` is not defined.

- [ ] **Step 3: Write `internal/gate/rpc.go`**

```go
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"

	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// UpdateRequest is the only request with input. The gate reads nothing else.
type UpdateRequest struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Auth   string `json:"auth"` // X-Registry-Auth for the pull; passed to Docker, never stored or logged
}

var (
	nameRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func (r UpdateRequest) Validate() error {
	if !nameRE.MatchString(r.Name) {
		return refuse("bad container name %q", r.Name)
	}
	if !digestRE.MatchString(r.Digest) {
		return refuse("bad digest %q", r.Digest)
	}
	return nil
}

// decode reads one small JSON object with no unknown fields.
func decode(body io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse("bad request: %v", err)
	}
	if dec.More() {
		return refuse("bad request: more than one object")
	}
	return nil
}

// Serve answers gate calls on l until ctx ends.
func (g *Gate) Serve(ctx context.Context, l net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /list", func(w http.ResponseWriter, r *http.Request) {
		ws, err := g.List(r.Context())
		reply(w, ws, err)
	})
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		var req UpdateRequest
		err := decode(r.Body, &req)
		if err == nil {
			err = req.Validate()
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		res, err := g.Update(r.Context(), req.Name, req.Digest, req.Auth)
		reply(w, res, err)
	})
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		evs, err := g.takeEvents()
		reply(w, evs, err)
	})
	return sock.Serve(ctx, l, mux)
}

// reply logs refusals loudly. The gate has no network, so its log is the
// record a hacked updater cannot hide.
func reply(w http.ResponseWriter, v any, err error) {
	var ref *RefusedError
	switch {
	case errors.As(err, &ref):
		log.Printf("REFUSED: %s", ref.Msg)
		http.Error(w, err.Error(), http.StatusForbidden)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (g *Gate) takeEvents() ([]state.Event, error) {
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	evs := st.Events
	st.Events = nil
	return evs, f.Save(st)
}

// Client calls the gate. The updater uses it.
type Client struct{ http *http.Client }

func Dial(socket string) *Client { return &Client{http: sock.Client(socket)} }

func (c *Client) List(ctx context.Context) ([]Watched, error) {
	var ws []Watched
	err := sock.Post(ctx, c.http, "http://gate/list", struct{}{}, &ws)
	return ws, err
}

func (c *Client) Update(ctx context.Context, name, digest, auth string) (Result, error) {
	var r Result
	err := sock.Post(ctx, c.http, "http://gate/update", UpdateRequest{Name: name, Digest: digest, Auth: auth}, &r)
	return r, err
}

func (c *Client) Events(ctx context.Context) ([]state.Event, error) {
	var evs []state.Event
	err := sock.Post(ctx, c.http, "http://gate/events", struct{}{}, &evs)
	return evs, err
}
```

- [ ] **Step 4: Run the tests and a short fuzz**

Run: `go test ./internal/gate/ -v`
Expected: PASS.

Run: `go test ./internal/gate/ -run '^$' -fuzz FuzzUpdateRequest -fuzztime 20s`
Expected: `PASS`, no failing inputs.

- [ ] **Step 5: Commit**

```bash
git add internal/gate/rpc.go internal/gate/rpc_test.go
git commit -m "Add gate RPC with strict request checks and fuzz test"
```

---

### Task 6: Crash recovery and updater spawn

**Files:**
- Create: `internal/gate/boot.go`, `internal/gate/boot_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 4.
- Produces:
  - `(*Gate).Recover(ctx) error`: fixes swaps left half-done and queues an event for each
  - `(*Gate).SpawnUpdater(ctx) error`
  - `(*Gate).RemoveUpdaters(ctx) error`
  - `updaterBody(self *docker.Container, dir string) map[string]any` (unexported, tested)

- [ ] **Step 1: Write the failing test**

`internal/gate/boot_test.go`:

```go
package gate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/docker"
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
	}
	b, _ := json.Marshal(updaterBody(self, "/run/bosun"))
	s := string(b)

	if strings.Contains(s, "docker.sock") {
		t.Errorf("updater must never get docker.sock: %s", s)
	}
	for _, want := range []string{
		`"Image":"sha256:img"`,
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
	for _, bad := range []string{"/srv/data", "SECRET=x", "PATH=/usr/bin"} {
		if strings.Contains(s, bad) {
			t.Errorf("updater body must not carry %s", bad)
		}
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/gate/ -run TestUpdaterBody`
Expected: FAIL. `updaterBody` is not defined.

- [ ] **Step 3: Write `internal/gate/boot.go`**

```go
package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// Recover finishes swaps that were cut off by a crash or power loss. A new
// container that runs and is healthy is kept. Otherwise the old one comes
// back. Each case leaves an event for the updater to send as a note.
func (g *Gate) Recover(ctx context.Context) error {
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range st.Pending {
		msg, err := g.recoverOne(ctx, p)
		if err != nil {
			msg = fmt.Sprintf("%s: crash recovery failed: %v. Check it by hand with `docker ps -a`", p.Name, err)
		}
		st.AddEvent("recovered", p.Name, msg)
	}
	st.Pending = nil
	return f.Save(st)
}

func (g *Gate) recoverOne(ctx context.Context, p state.Pending) (string, error) {
	cur, err := g.D.Inspect(ctx, p.Name)
	switch {
	case err != nil && !docker.IsNotFound(err):
		return "", err
	case err == nil && cur.ID == p.OldID:
		// Stopped or crashed before the rename. Nothing changed but the stop.
		return p.Name + ": an update was cut off before it changed anything; the container is running again", g.D.Start(ctx, p.OldID)
	case err == nil && cur.State.Running && (cur.State.Health == nil || cur.State.Health.Status == "healthy"):
		if err := g.D.Remove(ctx, p.OldID, true); err != nil && !docker.IsNotFound(err) {
			return "", err
		}
		return p.Name + ": an update was cut off; the new version is running and was kept", nil
	case err == nil:
		if err := g.D.Remove(ctx, cur.ID, true); err != nil {
			return "", err
		}
	}
	if err := g.D.Rename(ctx, p.OldID, p.Name); err != nil {
		return "", err
	}
	return p.Name + ": an update was cut off; the old version is back", g.D.Start(ctx, p.OldID)
}

// SpawnUpdater starts Bosun's updater container. It clears any old one first.
func (g *Gate) SpawnUpdater(ctx context.Context) error {
	self, err := g.D.Inspect(ctx, g.SelfID)
	if err != nil {
		return fmt.Errorf("find own container %q (do not set hostname on bosun-gate): %w", g.SelfID, err)
	}
	if err := g.RemoveUpdaters(ctx); err != nil {
		return err
	}
	id, err := g.D.Create(ctx, UpdaterName, updaterBody(self, g.Dir))
	if err != nil {
		return err
	}
	return g.D.Start(ctx, id)
}

// updaterBody is the updater's fixed container settings: the gate's image by
// ID (so a moved tag cannot swap it), no Docker socket, read-only, no
// capabilities. It gets the shared run folder and read-only copies of the
// gate's /etc/bosun mounts, and nothing else.
func updaterBody(self *docker.Container, dir string) map[string]any {
	binds := []string{}
	for _, m := range self.Mounts {
		src := m.Source
		if m.Type == "volume" {
			src = m.Name
		}
		switch {
		case strings.Contains(m.Source, "docker.sock"):
			continue // never, whatever the mount point
		case m.Destination == dir:
			binds = append(binds, src+":"+dir)
		case m.Destination == "/etc/bosun" || strings.HasPrefix(m.Destination, "/etc/bosun/"):
			binds = append(binds, src+":"+m.Destination+":ro")
		}
	}
	env := []string{"DOCKER_CONFIG=/etc/bosun/docker"}
	for _, e := range self.Config.Env {
		if strings.HasPrefix(e, "BOSUN_") {
			env = append(env, e)
		}
	}
	return map[string]any{
		"Image":  self.Image,
		"Cmd":    []string{"updater"},
		"Env":    env,
		"Labels": map[string]string{LabelManaged: "bosun-gate"},
		"HostConfig": map[string]any{
			"Binds":          binds,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"RestartPolicy":  map[string]string{"Name": "unless-stopped"},
			"NetworkMode":    "bridge",
		},
	}
}

// RemoveUpdaters removes every container the gate made.
func (g *Gate) RemoveUpdaters(ctx context.Context) error {
	sums, err := g.D.List(ctx, LabelManaged+"=bosun-gate", true)
	if err != nil {
		return err
	}
	for _, s := range sums {
		if err := g.D.Remove(ctx, s.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/gate/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gate/boot.go internal/gate/boot_test.go
git commit -m "Add crash recovery and locked-down updater spawn"
```

---

### Task 7: Registry checks

**Files:**
- Create: `internal/registry/registry.go`, `internal/registry/registry_test.go`

**Interfaces:**
- Produces:
  - `registry.New(insecure []string, caFile string) (*registry.Checker, error)`
  - `(*Checker).Digest(ctx, ref string) (string, error)`: the registry's digest for the tag, like `sha256:…`
  - `(*Checker).Auth(ref string) (string, error)`: the base64url `X-Registry-Auth` value, or `""` for anonymous

- [ ] **Step 1: Add the library**

```bash
go get github.com/google/go-containerregistry@v0.22.1
```

- [ ] **Step 2: Write the failing tests**

`internal/registry/registry_test.go`:

```go
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestDigest(t *testing.T) {
	// Keep this Mac's own Docker logins (and keychain helpers) out of the test.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(ggcrregistry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ref := u.Host + "/app:1"

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(name.MustParseReference(ref), img); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()

	c, err := New(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Digest(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Fatalf("Digest = %s, want %s", got, want)
	}
}

func TestAuthAnonymous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	c, _ := New(nil, "")
	a, err := c.Auth("example.com/app:1")
	if err != nil || a != "" {
		t.Fatalf("Auth = %q, %v; want empty", a, err)
	}
}

func TestAuthFromDockerConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	cfg := `{"auths":{"example.com":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:p")) + `"}}}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600)

	c, _ := New(nil, "")
	a, err := c.Auth("example.com/app:1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(a)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	json.Unmarshal(raw, &got)
	if got["username"] != "u" || got["password"] != "p" || got["serveraddress"] != "example.com" {
		t.Fatalf("auth = %v", got)
	}
}

func TestBadCAFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(p, []byte("not a cert"), 0o600)
	if _, err := New(nil, p); err == nil {
		t.Fatal("want an error for a CA file with no certificates")
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/registry/`
Expected: FAIL. `New` is not defined.

- [ ] **Step 4: Write `internal/registry/registry.go`**

```go
// Package registry asks registries for the current digest of a tag, and
// builds the login Docker needs to pull it.
package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Checker struct {
	insecure  map[string]bool
	transport http.RoundTripper
	keychain  authn.Keychain
}

// New makes a checker. insecure lists registries allowed over plain HTTP.
// caFile, if set, adds a CA for registries with private certificates.
// Logins come from $DOCKER_CONFIG/config.json.
func New(insecure []string, caFile string) (*Checker, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	set := map[string]bool{}
	for _, r := range insecure {
		set[r] = true
	}
	return &Checker{insecure: set, transport: t, keychain: authn.DefaultKeychain}, nil
}

func (c *Checker) parse(ref string) (name.Reference, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	if c.insecure[r.Context().RegistryStr()] {
		return name.ParseReference(ref, name.Insecure)
	}
	return r, nil
}

// Digest returns the registry's current digest for ref's tag. It uses HEAD,
// which does not count against Docker Hub pull limits.
func (c *Checker) Digest(ctx context.Context, ref string) (string, error) {
	r, err := c.parse(ref)
	if err != nil {
		return "", err
	}
	d, err := remote.Head(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain), remote.WithTransport(c.transport))
	if err != nil {
		return "", err
	}
	return d.Digest.String(), nil
}

// Auth returns the X-Registry-Auth value for pulling ref, or "" for anonymous.
func (c *Checker) Auth(ref string) (string, error) {
	r, err := c.parse(ref)
	if err != nil {
		return "", err
	}
	a, err := c.keychain.Resolve(r.Context())
	if err != nil {
		return "", err
	}
	if a == authn.Anonymous {
		return "", nil
	}
	ac, err := a.Authorization()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]string{
		"username":      ac.Username,
		"password":      ac.Password,
		"identitytoken": ac.IdentityToken,
		"serveraddress": r.Context().RegistryStr(),
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go mod tidy && go test ./internal/registry/ -v`
Expected: PASS, 4 tests.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/registry
git commit -m "Add registry digest checks and pull logins"
```

---

### Task 8: Updater

**Files:**
- Create: `internal/updater/updater.go`, `internal/updater/notify.go`, `internal/updater/updater_test.go`

**Interfaces:**
- Consumes: `gate.Watched`, `gate.Result`, `state.Event`, `sock.Serve`.
- Produces:
  - `updater.Gate` interface `{List; Update; Events}` (matches `*gate.Client`)
  - `updater.Registry` interface `{Digest(ctx, ref) (string, error); Auth(ref) (string, error)}` (matches `*registry.Checker`)
  - `updater.Updater{Gate Gate; Reg Registry; Notify func(string)}`
  - `(*Updater).Round(ctx, dryRun bool) ([]string, error)`, `(*Updater).Run(ctx, schedule string) error`, `(*Updater).Serve(ctx, l net.Listener) error`
  - `updater.ErrBusy`
  - `updater.LoadURLs(path string) ([]string, error)`, `updater.Sender(urls []string) func(string)`

- [ ] **Step 1: Add the libraries**

```bash
go get github.com/nicholas-fedor/shoutrrr@v0.20.0 github.com/robfig/cron/v3@v3.0.1
```

- [ ] **Step 2: Write the failing tests**

`internal/updater/updater_test.go`:

```go
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
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/updater/`
Expected: FAIL. `Updater` is not defined.

- [ ] **Step 4: Write `internal/updater/notify.go`**

```go
package updater

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/nicholas-fedor/shoutrrr"
)

// LoadURLs reads Shoutrrr URLs, one per line. Blank lines and # comments are
// skipped. A missing file means no notes.
func LoadURLs(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			urls = append(urls, l)
		}
	}
	return urls, nil
}

// Sender returns a notify func that sends msg to every URL. A failed note is
// logged only; it never blocks an update. URLs hold tokens, so they are
// never logged.
func Sender(urls []string) func(string) {
	return func(msg string) {
		log.Printf("note: %s", msg)
		for _, u := range urls {
			if err := shoutrrr.Send(u, msg); err != nil {
				scheme, _, _ := strings.Cut(u, ":")
				log.Printf("note to %s failed: %s", scheme, strings.ReplaceAll(err.Error(), u, scheme+"://…"))
			}
		}
	}
}
```

- [ ] **Step 5: Write `internal/updater/updater.go`**

```go
// Package updater checks registries on a schedule, asks the gate to update
// containers, and sends notes. It has network but no Docker socket.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// Gate is the part of *gate.Client the updater uses. It is an interface so
// tests can run rounds without Docker.
type Gate interface {
	List(ctx context.Context) ([]gate.Watched, error)
	Update(ctx context.Context, name, digest, auth string) (gate.Result, error)
	Events(ctx context.Context) ([]state.Event, error)
}

// Registry is the part of *registry.Checker the updater uses.
type Registry interface {
	Digest(ctx context.Context, ref string) (string, error)
	Auth(ref string) (string, error)
}

type Updater struct {
	Gate   Gate
	Reg    Registry
	Notify func(string)

	mu    sync.Mutex
	fails map[string]int    // registry failures in a row, per container
	told  map[string]string // notify mode: last digest we told about
}

var ErrBusy = errors.New("a round is running, try again later")

// Round checks every watched container once. dryRun only reports.
func (u *Updater) Round(ctx context.Context, dryRun bool) ([]string, error) {
	if !u.mu.TryLock() {
		return nil, ErrBusy
	}
	defer u.mu.Unlock()
	if u.fails == nil {
		u.fails, u.told = map[string]int{}, map[string]string{}
	}
	ws, err := u.Gate.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask the gate: %w", err)
	}
	lines := []string{}
	add := func(format string, a ...any) string {
		s := fmt.Sprintf(format, a...)
		lines = append(lines, s)
		return s
	}
	for _, w := range ws {
		d, err := u.Reg.Digest(ctx, w.Ref)
		if err != nil {
			add("%s: registry check failed: %v", w.Name, err)
			if !dryRun {
				if u.fails[w.Name]++; u.fails[w.Name] == 3 {
					u.Notify(fmt.Sprintf("%s: registry check failed 3 rounds in a row: %v", w.Name, err))
				}
			}
			continue
		}
		u.fails[w.Name] = 0
		switch {
		case slices.Contains(w.Digests, d):
			add("%s: up to date", w.Name)
		case slices.Contains(w.Skip, d):
			add("%s: the new version is on the skip list", w.Name)
		case w.Mode == "notify":
			msg := add("%s: a new version of %s is ready (notify only)", w.Name, w.Ref)
			if !dryRun && u.told[w.Name] != d {
				u.told[w.Name] = d
				u.Notify(msg)
			}
		case dryRun:
			add("%s: would update %s", w.Name, w.Ref)
		default:
			auth, err := u.Reg.Auth(w.Ref)
			if err != nil {
				u.Notify(add("%s: could not read the registry login: %v", w.Name, err))
				continue
			}
			res, err := u.Gate.Update(ctx, w.Name, d, auth)
			if err != nil {
				u.Notify(add("%s: update failed: %v", w.Name, err))
				continue
			}
			u.Notify(add("%s", res.Message))
		}
	}
	if !dryRun {
		u.sendEvents(ctx)
	}
	return lines, nil
}

func (u *Updater) sendEvents(ctx context.Context) {
	evs, err := u.Gate.Events(ctx)
	if err != nil {
		log.Printf("read gate events: %v", err)
		return
	}
	for _, e := range evs {
		u.Notify(e.Message)
	}
}

// Run sends waiting events, then runs a round at each cron time until ctx ends.
func (u *Updater) Run(ctx context.Context, schedule string) error {
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return fmt.Errorf("BOSUN_SCHEDULE %q: %w", schedule, err)
	}
	u.sendEvents(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(sched.Next(time.Now()))):
		}
		lines, err := u.Round(ctx, false)
		for _, l := range lines {
			log.Println(l)
		}
		if err != nil {
			log.Printf("round: %v", err)
		}
	}
}

// Serve answers `bosun check` from the gate's CLI on l.
func (u *Updater) Serve(ctx context.Context, l net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /round", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DryRun bool `json:"dry_run"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lines, err := u.Round(context.WithoutCancel(r.Context()), req.DryRun)
		switch {
		case errors.Is(err, ErrBusy):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			_ = json.NewEncoder(w).Encode(lines)
		}
	})
	return sock.Serve(ctx, l, mux)
}
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `go mod tidy && go test ./internal/updater/ -v && go vet ./...`
Expected: PASS, 8 tests.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/updater
git commit -m "Add updater rounds, schedule, and Shoutrrr notes"
```

---

### Task 9: Command line

**Files:**
- Create: `main.go`, `cli.go`, `cli_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: the `bosun` binary with commands `gate`, `updater`, `status`, `check [--dry-run]`, `rollback ls|show <name>|<name> [--dry-run] [--yes]`, `skip ls|clear <name>`, `help [command]`.

- [ ] **Step 1: Write the failing test**

`cli_test.go`:

```go
package main

import (
	"reflect"
	"testing"
	"time"
)

func TestSplitArgs(t *testing.T) {
	pos, flags := splitArgs([]string{"nginx", "--yes", "--dry-run"})
	if !reflect.DeepEqual(pos, []string{"nginx"}) || !flags["yes"] || !flags["dry-run"] {
		t.Fatalf("got %v %v", pos, flags)
	}
	if err := checkFlags(flags, "yes"); err == nil {
		t.Fatal("want an error for --dry-run when only --yes is allowed")
	}
}

func TestAgo(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{72 * time.Hour, "3 days ago"},
	} {
		if got := ago(time.Now().Add(-tc.d)); got != tc.want {
			t.Errorf("ago(-%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if ago(time.Time{}) != "never" {
		t.Error("zero time must be never")
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test .`
Expected: FAIL. `splitArgs` is not defined.

- [ ] **Step 3: Write `main.go`**

```go
// Command bosun keeps Docker containers up to date without giving the
// network-facing code the Docker socket.
// Design: docs/superpowers/specs/2026-09-11-bosun-design.md
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/registry"
	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/updater"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	dockerSock = env("BOSUN_DOCKER_SOCK", "/var/run/docker.sock")
	runDir     = env("BOSUN_RUN_DIR", "/run/bosun")
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "gate":
		return runGate(ctx)
	case "updater":
		return runUpdater(ctx)
	case "status":
		return cmdStatus(ctx)
	case "check":
		return cmdCheck(ctx, args)
	case "rollback":
		return cmdRollback(ctx, args)
	case "skip":
		return cmdSkip(args)
	case "help", "-h", "--help":
		return cmdHelp(args)
	}
	return fmt.Errorf("unknown command %q. Run `bosun help`", cmd)
}

func newGate() *gate.Gate {
	self, _ := os.Hostname() // Docker sets it to the short container ID
	return &gate.Gate{D: docker.New(dockerSock), Dir: runDir, SelfID: self}
}

func runGate(ctx context.Context) error {
	g := newGate()
	if err := g.Recover(ctx); err != nil {
		return fmt.Errorf("crash recovery: %w", err)
	}
	// Listen before starting the updater, so its first call waits for us.
	l, err := sock.Listen(filepath.Join(runDir, "gate.sock"))
	if err != nil {
		return err
	}
	if err := g.SpawnUpdater(ctx); err != nil {
		return fmt.Errorf("start the updater: %w", err)
	}
	defer func() {
		if err := g.RemoveUpdaters(context.Background()); err != nil {
			log.Printf("remove the updater: %v", err)
		}
	}()
	log.Printf("gate ready")
	return g.Serve(ctx, l)
}

func runUpdater(ctx context.Context) error {
	reg, err := registry.New(splitList(os.Getenv("BOSUN_INSECURE_REGISTRIES")), os.Getenv("BOSUN_CA_FILE"))
	if err != nil {
		return err
	}
	urls, err := updater.LoadURLs(env("BOSUN_NOTIFY_FILE", "/etc/bosun/notify.txt"))
	if err != nil {
		return err
	}
	u := &updater.Updater{Gate: gate.Dial(filepath.Join(runDir, "gate.sock")), Reg: reg, Notify: updater.Sender(urls)}
	l, err := sock.Listen(filepath.Join(runDir, "updater.sock"))
	if err != nil {
		return err
	}
	go func() {
		if err := u.Serve(ctx, l); err != nil {
			log.Printf("updater socket: %v", err)
		}
	}()
	schedule := env("BOSUN_SCHEDULE", "0 4 * * *")
	log.Printf("updater ready: %d notify URLs, schedule %q", len(urls), schedule)
	return u.Run(ctx, schedule)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 4: Write `cli.go`**

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/state"
)

var help = map[string]string{
	"status": "bosun status\n  Show watched containers: mode, image, last update, downtime, and whether a rollback is kept.\n" +
		"  Example: docker exec -it bosun-gate bosun status",
	"check": "bosun check [--dry-run]\n  Run an update round now. --dry-run only prints what it would do.\n" +
		"  Example: docker exec -it bosun-gate bosun check --dry-run",
	"rollback": "bosun rollback ls\nbosun rollback show <name>\nbosun rollback <name> [--dry-run] [--yes]\n" +
		"  Go back to the version kept by the last update. It asks before it acts; --yes skips the question.\n" +
		"  Example: docker exec -it bosun-gate bosun rollback nginx",
	"skip": "bosun skip ls\nbosun skip clear <name>\n  List or clear versions Bosun will not update to.\n" +
		"  Example: docker exec -it bosun-gate bosun skip clear nginx",
}

func usage() {
	fmt.Println("usage: bosun <status|check|rollback|skip|help> ...\nRun `bosun help <command>` for one command.")
}

func cmdHelp(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	h, ok := help[args[0]]
	if !ok {
		return fmt.Errorf("no help for %q. Commands: status, check, rollback, skip", args[0])
	}
	fmt.Println(h)
	return nil
}

// splitArgs separates --flags from names, so `rollback nginx --yes` works.
// ponytail: no flag values are needed yet; add them when a flag takes one.
func splitArgs(args []string) ([]string, map[string]bool) {
	var pos []string
	flags := map[string]bool{}
	for _, a := range args {
		if f, ok := strings.CutPrefix(a, "--"); ok {
			flags[f] = true
		} else {
			pos = append(pos, a)
		}
	}
	return pos, flags
}

func checkFlags(flags map[string]bool, allowed ...string) error {
	for f := range flags {
		if !slices.Contains(allowed, f) {
			return fmt.Errorf("unknown flag --%s. Run `bosun help`", f)
		}
	}
	return nil
}

func cmdStatus(ctx context.Context) error {
	ws, err := newGate().List(ctx)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		fmt.Println("No watched containers. Add the label bosun.enable=true to a container.")
		return nil
	}
	st, err := state.Read(runDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODE\tIMAGE\tLAST UPDATE\tDOWNTIME\tROLLBACK KEPT")
	for _, w := range ws {
		e := st.Entry(w.Name)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", w.Name, w.Mode, w.Ref, ago(e.UpdatedAt), dur(e.Downtime), yesNo(e.Prev != ""))
	}
	return tw.Flush()
}

func cmdCheck(ctx context.Context, args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags, "dry-run"); err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("check takes no names. Run `bosun help check`")
	}
	var lines []string
	c := sock.Client(filepath.Join(runDir, "updater.sock"))
	if err := sock.Post(ctx, c, "http://updater/round", map[string]bool{"dry_run": flags["dry-run"]}, &lines); err != nil {
		return fmt.Errorf("ask the updater: %w. Is it running? See `docker logs bosun-updater`", err)
	}
	if len(lines) == 0 {
		fmt.Println("No watched containers. Add the label bosun.enable=true to a container.")
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// ponytail: a container named "ls" or "show" cannot be rolled back by name.
func cmdRollback(ctx context.Context, args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags, "dry-run", "yes"); err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("which container? Run `bosun rollback ls` to see what you can roll back")
	}
	st, err := state.Read(runDir)
	if err != nil {
		return err
	}
	switch pos[0] {
	case "ls":
		return rollbackLs(ctx, st)
	case "show":
		if len(pos) < 2 {
			return errors.New("usage: bosun rollback show <name>")
		}
		return rollbackShow(ctx, st, pos[1])
	}
	name := pos[0]
	if err := rollbackShow(ctx, st, name); err != nil {
		return err
	}
	if flags["dry-run"] {
		fmt.Println("Dry run: nothing changed.")
		return nil
	}
	if !flags["yes"] && !confirm("Continue? [y/N] ") {
		return errors.New("stopped; nothing changed")
	}
	res, err := newGate().Rollback(ctx, name)
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	if res.Status == gate.StatusReverted {
		return errors.New("the rollback did not happen")
	}
	return nil
}

func rollbackLs(ctx context.Context, st *state.State) error {
	d := docker.New(dockerSock)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNOW\tBACK TO\tUPDATED")
	n := 0
	for _, name := range sortedNames(st) {
		e := st.Containers[name]
		if e.Prev == "" {
			continue
		}
		now := "?"
		if c, err := d.Inspect(ctx, name); err == nil {
			now = c.Config.Image + " (" + short(c.Image) + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, now, e.Prev, ago(e.UpdatedAt))
		n++
	}
	if n == 0 {
		fmt.Println("Nothing to roll back yet. Bosun keeps one old version after each update.")
		return nil
	}
	return tw.Flush()
}

func rollbackShow(ctx context.Context, st *state.State, name string) error {
	e := st.Containers[name]
	if e == nil || e.Prev == "" {
		return fmt.Errorf("no old version kept for %s. Run `bosun rollback ls` to see what you can roll back", name)
	}
	c, err := docker.New(dockerSock).Inspect(ctx, name)
	if err != nil {
		return err
	}
	fmt.Printf("%s now runs %s (image %s).\n", name, c.Config.Image, short(c.Image))
	fmt.Printf("A rollback puts back %s, from the update %s.\n", e.Prev, ago(e.UpdatedAt))
	fmt.Println("Volumes are not changed. Data written by the newer version stays.")
	fmt.Println("The newer version goes on the skip list.")
	fmt.Printf("To do it: docker exec -it bosun-gate bosun rollback %s\n", name)
	return nil
}

func cmdSkip(args []string) error {
	pos, flags := splitArgs(args)
	if err := checkFlags(flags); err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && pos[0] == "ls":
		st, err := state.Read(runDir)
		if err != nil {
			return err
		}
		n := 0
		for _, name := range sortedNames(st) {
			for _, d := range st.Containers[name].Skip {
				fmt.Printf("%s\t%s\n", name, d)
				n++
			}
		}
		if n == 0 {
			fmt.Println("Nothing is on the skip list.")
		}
		return nil
	case len(pos) == 2 && pos[0] == "clear":
		f, st, err := state.Open(runDir, false)
		if err != nil {
			return err
		}
		defer f.Close()
		e := st.Containers[pos[1]]
		if e == nil || len(e.Skip) == 0 {
			return fmt.Errorf("nothing is skipped for %s. Run `bosun skip ls`", pos[1])
		}
		e.Skip = nil
		if err := f.Save(st); err != nil {
			return err
		}
		fmt.Printf("%s: skip list cleared. The next round may update it again.\n", pos[1])
		return nil
	}
	return errors.New("usage: bosun skip ls | bosun skip clear <name>")
}

func confirm(q string) bool {
	fmt.Print(q)
	a, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a = strings.ToLower(strings.TrimSpace(a))
	return a == "y" || a == "yes"
}

func sortedNames(st *state.State) []string {
	names := make([]string, 0, len(st.Containers))
	for n := range st.Containers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func dur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(100 * time.Millisecond).String()
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
```

- [ ] **Step 5: Run the tests and build**

Run: `go test ./... && go vet ./... && go build -o /tmp/bosun . && /tmp/bosun help rollback`
Expected: PASS, then the rollback help text.

- [ ] **Step 6: Commit**

```bash
git add main.go cli.go cli_test.go
git commit -m "Add bosun command line"
```

---

### Task 10: Full-flow tests on real Docker

These run on this Mac (OrbStack or Docker Desktop) and in CI. They need a Docker socket and the `docker` CLI.

**Files:**
- Create: `internal/gate/integration_test.go`

**Interfaces:**
- Consumes: `gate.Gate` (Update, Rollback, Recover), `docker.New`, `registry.New`, `state.Open`.

- [ ] **Step 1: Write the tests**

`internal/gate/integration_test.go`:

```go
//go:build integration

package gate_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/registry"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const repo = "localhost:5000/bosun-test/app"

const (
	v1     = "FROM busybox:1.36\nRUN echo v1 > /version\nCMD [\"sleep\", \"3600\"]\n"
	v2     = "FROM busybox:1.36\nRUN echo v2 > /version\nCMD [\"sleep\", \"3600\"]\n"
	broken = "FROM busybox:1.36\nRUN echo broken > /version\nCMD [\"false\"]\n"
	newCmd = "FROM busybox:1.36\nRUN echo v2 > /version\nCMD [\"sleep\", \"7200\"]\n"
)

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// push builds dockerfile, pushes it as repo:latest, drops the local tag so the
// gate must really pull, and returns the registry digest.
func push(t *testing.T, dockerfile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, "docker", "build", "-q", "-t", repo+":latest", dir)
	sh(t, "docker", "push", "-q", repo+":latest")
	sh(t, "docker", "image", "rm", repo+":latest")
	reg, err := registry.New(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := reg.Digest(context.Background(), repo+":latest")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func setup(t *testing.T) (*gate.Gate, string) {
	t.Helper()
	if exec.Command("docker", "inspect", "bosun-test-registry").Run() != nil {
		sh(t, "docker", "run", "-d", "--rm", "-p", "5000:5000", "--name", "bosun-test-registry", "registry:2")
	}
	for i := 0; ; i++ {
		resp, err := http.Get("http://localhost:5000/v2/")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i == 40 {
			t.Fatalf("registry did not start: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	name := "bosun-it-" + strings.ToLower(t.Name())
	t.Cleanup(func() {
		ids, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+name).Output()
		if f := strings.Fields(string(ids)); len(f) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, f...)...).Run()
		}
	})
	sockPath := os.Getenv("BOSUN_DOCKER_SOCK")
	if sockPath == "" {
		sockPath = "/var/run/docker.sock"
	}
	return &gate.Gate{D: docker.New(sockPath), Dir: t.TempDir(), Poll: 200 * time.Millisecond}, name
}

func runApp(t *testing.T, name string) {
	sh(t, "docker", "run", "-d", "--name", name,
		"--label", "bosun.enable=true", "--label", "bosun.health-timeout=3s", repo+":latest")
}

func version(t *testing.T, name string) string {
	return sh(t, "docker", "exec", name, "cat", "/version")
}

func TestUpdateRevertSkipAndRollback(t *testing.T) {
	g, name := setup(t)
	ctx := context.Background()
	push(t, v1)
	runApp(t, name)

	d2 := push(t, v2)
	res, err := g.Update(ctx, name, d2, "")
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("update to v2: %+v %v", res, err)
	}
	if v := version(t, name); v != "v2" {
		t.Fatalf("running %s, want v2", v)
	}

	d3 := push(t, broken)
	res, err = g.Update(ctx, name, d3, "")
	if err != nil || res.Status != gate.StatusReverted {
		t.Fatalf("broken update: %+v %v", res, err)
	}
	if v := version(t, name); v != "v2" {
		t.Fatalf("after a failed update running %s, want v2", v)
	}
	if _, err := g.Update(ctx, name, d3, ""); err == nil || !strings.Contains(err.Error(), "skip list") {
		t.Fatalf("a skipped digest must be refused, got %v", err)
	}

	res, err = g.Rollback(ctx, name)
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("after rollback running %s, want v1", v)
	}
	if out := sh(t, "docker", "ps", "-a", "--filter", "name="+name+"-bosun-", "-q"); out != "" {
		t.Fatalf("old containers left behind: %s", out)
	}
}

func TestNewImageCmdIsUsed(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	d := push(t, newCmd)
	if res, err := g.Update(context.Background(), name, d, ""); err != nil || res.Status != gate.StatusDone {
		t.Fatalf("update: %+v %v", res, err)
	}
	if cmd := sh(t, "docker", "inspect", "-f", "{{json .Config.Cmd}}", name); cmd != `["sleep","7200"]` {
		t.Fatalf("Cmd = %s, want the new image's CMD", cmd)
	}
}

// crash leaves name as a stopped, renamed old container, as if the gate died
// mid-swap, and records it as pending.
func crash(t *testing.T, g *gate.Gate, name string) string {
	t.Helper()
	oldID := sh(t, "docker", "inspect", "-f", "{{.Id}}", name)
	sh(t, "docker", "stop", "-t", "1", name)
	sh(t, "docker", "rename", name, name+"-bosun-abc123")
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending = append(st.Pending, state.Pending{Name: name, OldID: oldID, TmpName: name + "-bosun-abc123"})
	if err := f.Save(st); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return oldID
}

func TestRecoverRestoresOld(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	crash(t, g, name)
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("running %s, want the old v1 back", v)
	}
	st, _ := state.Read(g.Dir)
	if len(st.Pending) != 0 || len(st.Events) != 1 || !strings.Contains(st.Events[0].Message, "old version is back") {
		t.Fatalf("state = %+v", st)
	}
}

func TestRecoverKeepsHealthyNew(t *testing.T) {
	g, name := setup(t)
	push(t, v1)
	runApp(t, name)
	oldID := crash(t, g, name)
	runApp(t, name) // the "new" container came up before the crash
	if err := g.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exec.Command("docker", "inspect", oldID).Run() == nil {
		t.Fatal("old container must be removed")
	}
	st, _ := state.Read(g.Dir)
	if len(st.Events) != 1 || !strings.Contains(st.Events[0].Message, "new version is running") {
		t.Fatalf("events = %+v", st.Events)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test -tags integration -count=1 -v ./internal/gate/`
Expected: PASS, 4 integration tests plus the unit tests. The first run pulls `busybox` and `registry:2`.

If Docker says `http: server gave HTTP response to HTTPS client` for `localhost:5000`, the daemon does not treat localhost as insecure. Add `"insecure-registries": ["localhost:5000"]` to the Docker daemon settings (OrbStack: Settings → Docker; Docker Desktop: Settings → Docker Engine), restart Docker, and run again.

- [ ] **Step 3: Fix what fails, then commit**

```bash
git add internal/gate/integration_test.go
git commit -m "Add full-flow tests on real Docker"
```

---

### Task 11: Image, compose, README, CI, and smoke test

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `compose.yml`, `README.md`, `.github/workflows/ci.yml`

- [ ] **Step 1: Write `Dockerfile`**

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bosun . \
 && mkdir -m 0700 -p /out/run/bosun

# Distroless: no shell, no package manager. nonroot is UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bosun /usr/local/bin/bosun
# The shared run folder. A new named volume copies this owner and mode.
COPY --from=build --chown=65532:65532 /out/run/bosun /run/bosun
ENTRYPOINT ["/usr/local/bin/bosun"]
CMD ["gate"]
```

`.dockerignore`:

```
.git
docs
```

- [ ] **Step 2: Write `compose.yml`**

```yaml
# Bosun: one service. The gate starts its own updater container.
# Set DOCKER_GID first (see README).
services:
  bosun:
    build: .
    container_name: bosun-gate
    restart: unless-stopped
    network_mode: none
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    group_add: ["${DOCKER_GID:?set DOCKER_GID, see README}"]
    environment:
      BOSUN_SCHEDULE: "0 4 * * *"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - bosun-run:/run/bosun
      # Optional. Everything under /etc/bosun is passed to the updater read-only.
      # - ./notify.txt:/etc/bosun/notify.txt:ro
      # - ./registry-auth.json:/etc/bosun/docker/config.json:ro

volumes:
  bosun-run:
```

- [ ] **Step 3: Write `README.md`**

````markdown
# Bosun

Bosun keeps your Docker containers up to date. It replaces Watchtower.

**The main difference:** the part that talks to the internet never touches the Docker
socket. The part that touches the socket has no network, and it can only swap a
container's image. Even if the network part is hacked, it cannot take over your host.

- Opt-in: only containers with `bosun.enable=true` are touched.
- Rolls back by itself when a new version is not healthy.
- Manual rollback with one command.
- Notes to Slack, Teams, Discord, ntfy, email and more (Shoutrrr URLs).

## Install

1. Find the group that owns the Docker socket, as containers see it:

   ```bash
   export DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g /s)
   ```

2. Start Bosun:

   ```bash
   docker compose up -d --build
   ```

   You will see two containers. `bosun-gate` is yours. `bosun-updater` is made by the
   gate, and the gate removes it when it stops.

Do not set `hostname` or `user` on the gate. It finds its own container by hostname.

## Watch a container

| Label | Default | Meaning |
|---|---|---|
| `bosun.enable=true` | off | Watch this container. |
| `bosun.mode=notify` | `update` | Tell only. Do not update. |
| `bosun.health-timeout=120s` | `60s` | How long a new version has to become healthy. |

Bosun follows the tag. `postgres:16` stays on 16. Images pinned by digest are never
touched.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `BOSUN_SCHEDULE` | `0 4 * * *` | Cron string for update rounds. |
| `BOSUN_NOTIFY_FILE` | `/etc/bosun/notify.txt` | Shoutrrr URLs, one per line. |
| `BOSUN_INSECURE_REGISTRIES` | none | Comma list of registries allowed over plain HTTP. |
| `BOSUN_CA_FILE` | none | CA certificate for registries with private certificates. |

Registry logins: mount a Docker `config.json` at `/etc/bosun/docker/config.json`. Use a
read-only token made just for Bosun (for GHCR: only `read:packages`). Do not mount your
own `~/.docker/config.json`.

## Commands

```bash
docker exec -it bosun-gate bosun status
docker exec -it bosun-gate bosun check --dry-run
docker exec -it bosun-gate bosun rollback ls
docker exec -it bosun-gate bosun rollback nginx
docker exec -it bosun-gate bosun skip clear nginx
docker exec -it bosun-gate bosun help rollback
```

## Limits

- A rollback swaps the image only. It does not undo data changes.
- Bosun recreates containers made by compose outside of compose. A later
  `docker compose up` may recreate them again. That is harmless.
- Bosun does not update itself yet. It needs Docker 25 or newer.
````

- [ ] **Step 4: Write `.github/workflows/ci.yml`**

```yaml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test ./...
      - run: go test -run '^$' -fuzz FuzzUpdateRequest -fuzztime 30s ./internal/gate
      - run: go test -tags integration -count=1 ./...
      - run: docker build -t bosun:ci .
```

- [ ] **Step 5: Smoke test on this Mac**

```bash
export DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g /s)
```

```bash
docker compose up -d --build
```

```bash
docker run -d --name bosun-smoke --label bosun.enable=true nginx:alpine
```

Check each of these:

```bash
docker ps --filter name=bosun
```

Expected: `bosun-gate`, `bosun-updater` and `bosun-smoke` are running.

```bash
docker inspect bosun-updater --format '{{json .HostConfig.Binds}} {{.HostConfig.ReadonlyRootfs}} {{json .HostConfig.CapDrop}}'
```

Expected: no `docker.sock` in the list, `true`, `["ALL"]`.

```bash
docker exec -it bosun-gate bosun status
```

Expected: a row for `bosun-smoke` in `update` mode.

```bash
docker exec -it bosun-gate bosun check --dry-run
```

Expected: `bosun-smoke: up to date`.

```bash
docker compose down
```

```bash
docker ps -a --filter name=bosun-updater -q
```

Expected: empty. The gate removed its updater.

```bash
docker rm -f bosun-smoke
```

If `status` says `permission denied` on the socket, `DOCKER_GID` is wrong. Run step 1
of the README again.

- [ ] **Step 6: Commit**

```bash
git add Dockerfile .dockerignore compose.yml README.md .github
git commit -m "Add image, compose file, README and CI"
```

- [ ] **Step 7: Update the vault**

In `/Users/mehrad/Library/Mobile Documents/iCloud~md~obsidian/Documents/Notes/Projects/Bosun/Bosun.md`, set the Status section to: core built and smoke-tested; next is plan 2 (backups and warnings). In `Projects.md`, update the Bosun row the same way, set its date to the last commit date, and bump `updated:`.
