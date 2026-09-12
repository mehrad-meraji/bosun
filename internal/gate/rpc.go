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

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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

// IsRefused reports whether the gate refused a call. The updater tells a
// refusal from a failure this way, because they are different events.
func IsRefused(err error) bool {
	var ref *RefusedError
	if errors.As(err, &ref) {
		return true
	}
	var he *sock.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusForbidden
}

// IsBusy reports whether the gate was busy with an update. The caller may
// try again later, so it is not a failure. Over the socket it arrives as a
// 409, like the updater's own busy reply.
func IsBusy(err error) bool {
	if errors.Is(err, state.ErrBusy) {
		return true
	}
	var he *sock.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusConflict
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
