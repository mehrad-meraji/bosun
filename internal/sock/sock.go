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

// shutdownWait is how long Serve waits for running calls after ctx ends. A
// swap can take a while; cutting it off is worse than a slow stop. Keep it
// under stop_grace_period in compose.yml.
const shutdownWait = 150 * time.Second

// Serve runs h on l until ctx ends, then waits for running calls to finish
// (up to shutdownWait) before it returns.
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		err := srv.Shutdown(sctx)
		if err != nil {
			srv.Close()
		}
		done <- err
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}

// HTTPError is a reply with a status other than 200. Msg is the reply text,
// so an error reads the same as before; Status lets a caller tell a refusal
// (403) from a failure (500).
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

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
		return &HTTPError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	return nil
}
