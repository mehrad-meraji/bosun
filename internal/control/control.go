// Package control talks to an optional control server, for example Sentinel.
// It sends events and reads a short list of commands. Only the updater uses
// it: the gate has no network and opens no port, so the server can never
// connect to Bosun.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mehrad-meraji/bosun/internal/gate"
)

// Version is Bosun's version. It goes out with every event. Bump it by hand.
const Version = "0.2.0"

// Schema is the event format version. Bump it when a field changes meaning.
const Schema = 1

// Event types.
const (
	EventUpdateDone       = "update.done"
	EventUpdateRolledBack = "update.rolled_back"
	EventUpdateFailed     = "update.failed"
	EventVersionAvailable = "version.available"
	EventGateRefused      = "gate.refused"
	EventRegistryFailing  = "registry.failing"
	EventRecovery         = "recovery"
	EventWarning          = "warning"
	EventCommandResult    = "command.result"
)

// Backup is the size and time of the backup an update took.
type Backup struct {
	Bytes int64 `json:"bytes"`
	Ms    int64 `json:"ms"`
}

// Event is one thing that happened. The server drops repeats by ID. The
// body never holds registry logins, notify URLs or env vars.
type Event struct {
	Schema     int         `json:"schema"`
	ID         string      `json:"id"`
	Time       time.Time   `json:"time"`
	Host       string      `json:"host"`
	Version    string      `json:"bosun_version"`
	Type       string      `json:"type"`
	Container  string      `json:"container,omitempty"`
	Image      string      `json:"image,omitempty"`
	FromDigest string      `json:"from_digest,omitempty"`
	ToDigest   string      `json:"to_digest,omitempty"`
	DowntimeMs int64       `json:"downtime_ms,omitempty"`
	Backup     *Backup     `json:"backup,omitempty"`
	Steps      []gate.Step `json:"steps,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	CommandID  string      `json:"command_id,omitempty"`
}

// Client sends events and reads commands. A nil Client means the link is off.
type Client struct {
	url   string // base URL, no trailing slash
	token string
	host  string
	http  *http.Client

	seen map[string]time.Time // command IDs already run; see commands.go
}

// New returns nil when rawURL is empty: the link is off. The token comes
// from a file, like the notify URLs, so it is not in the env.
func New(rawURL, tokenFile, host string, insecure bool) (*Client, error) {
	if rawURL == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q: %w", rawURL, err)
	}
	switch {
	case u.Host == "":
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q has no host; use https://your-server", rawURL)
	case u.Scheme == "https":
	case u.Scheme == "http" && insecure:
	default:
		return nil, fmt.Errorf("BOSUN_CONTROL_URL %q must start with https://. For a server on your own LAN, set BOSUN_CONTROL_INSECURE=true", rawURL)
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the control token from %s: %w. Mount the file under /etc/bosun, or unset BOSUN_CONTROL_URL", tokenFile, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("the control token file %s is empty; put the server's token in it", tokenFile)
	}
	if host == "" {
		return nil, fmt.Errorf("the control link needs a host name, and Docker gave none; set BOSUN_HOST on bosun-gate")
	}
	return &Client{
		url:   strings.TrimSuffix(rawURL, "/"),
		token: token,
		host:  host,
		http:  &http.Client{Timeout: 20 * time.Second},
		seen:  map[string]time.Time{},
	}, nil
}

// Host is the name this Bosun reports as.
func (c *Client) Host() string { return c.host }

// retryWaits are the waits between tries. A var, so tests can shorten them.
var retryWaits = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// statusError is a reply with a status of 300 or more. The reply body is
// never kept: a server could echo the token back in it.
type statusError struct{ status int }

func (e *statusError) Error() string {
	return fmt.Sprintf("control server: %d %s", e.status, http.StatusText(e.status))
}

// Send posts one event. It tries up to four times: at once, then after 5 s,
// 30 s and 2 min. A bad request or a bad token is not tried again, because
// it will not fix itself. The caller logs and drops what comes back.
func (c *Client) Send(ctx context.Context, ev Event) error {
	ev.Schema, ev.Version = Schema, Version
	if ev.ID == "" {
		ev.ID = newID()
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Host == "" {
		ev.Host = c.host
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	for i := 0; ; i++ {
		err = c.post(ctx, "/events", b)
		if err == nil {
			return nil
		}
		var se *statusError
		if errors.As(err, &se) && se.status < 500 && se.status != http.StatusTooManyRequests {
			return err
		}
		if i >= len(retryWaits) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryWaits[i]):
		}
	}
}

func (c *Client) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control server: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return &statusError{status: resp.StatusCode}
	}
	return nil
}

// newID makes a random UUID for one event.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()) // ponytail: crypto/rand does not fail in practice; a clock id still lets the server drop repeats
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
