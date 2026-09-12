// Package docker is a small Docker Engine API client.
// ponytail: stdlib over the unix socket, not the Docker SDK. Bosun needs a
// dozen calls, and raw JSON lets recreate copy HostConfig without dropping
// fields a typed SDK does not know about.
package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
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

// Info returns the Docker host's name, which Bosun uses as the host name in
// control-server events.
func (c *Client) Info(ctx context.Context) (string, error) {
	var out struct{ Name string }
	if err := c.call(ctx, http.MethodGet, "/info", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Name, nil
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

// Archive streams a tar of path from a container, running or stopped. Docker
// names each entry after the last part of path. The caller closes it.
func (c *Client) Archive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {path}}, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Wait blocks until the container stops and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := c.call(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil, &out); err != nil {
		return 0, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// multiplexed is the content type Docker uses for a container started
// without a TTY: the output comes in chunks behind an 8-byte header.
const multiplexed = "application/vnd.docker.multiplexed-stream"

const (
	logLines = 50      // lines asked of Docker
	logRead  = 1 << 20 // most bytes read from Docker
	logKeep  = 4096    // most bytes returned
)

// Logs returns the last lines a container printed: 50 lines, and if those are
// longer than 4 KB, their last 4 KB. The end matters most, because that is
// where a crash says why.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(logLines)}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, logRead))
	if framed(resp.Header.Get("Content-Type")) {
		b = deframe(b)
	}
	return strings.TrimSpace(string(lastBytes(b))), err
}

// framed reports whether the body carries Docker's chunk headers. Docker
// names the format, so there is nothing to guess.
func framed(contentType string) bool {
	t, _, err := mime.ParseMediaType(contentType)
	return err == nil && t == multiplexed
}

// deframe drops Docker's 8-byte chunk headers. A read cut short leaves a
// chunk shorter than its header says, so whatever arrived is kept.
func deframe(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if n < 0 || n > len(b) { // a length field cannot be trusted
			n = len(b)
		}
		out = append(out, b[:n]...)
		b = b[n:]
	}
	return out
}

// lastBytes keeps the last logKeep bytes, starting at a line break so the
// first line is whole.
func lastBytes(b []byte) []byte {
	if len(b) <= logKeep {
		return b
	}
	b = b[len(b)-logKeep:]
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
		b = b[i+1:]
	}
	return b
}

// SplitRef splits "host:5000/app:1.2" into ("host:5000/app", "1.2").
// No tag means "latest".
func SplitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
