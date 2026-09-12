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

	"github.com/mehrad-meraji/bosun/internal/control"
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
	SkipClear(ctx context.Context, name string) (gate.SkipClearResult, error)
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

	// Control is the control server link, or nil when it is off.
	Control *control.Client

	evs chan control.Event // events waiting to go out

	mu    sync.Mutex
	fails map[string]int    // registry failures in a row, per container
	told  map[string]string // notify mode: last digest we told about
}

var ErrBusy = errors.New("a round is running, try again later")

// eventQueue is how many events wait to go out. A slow server must never
// hold up an update, so a full queue drops the newest news: the log keeps it.
const eventQueue = 100

// StartEvents starts the one goroutine that sends events. It does nothing
// when the link is off. Call it once, before Run.
func (u *Updater) StartEvents(ctx context.Context) {
	if u.Control == nil {
		return
	}
	u.evs = make(chan control.Event, eventQueue)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-u.evs:
				if err := u.Control.Send(ctx, ev); err != nil && ctx.Err() == nil {
					log.Printf("control event %s dropped: %v", ev.Type, err)
				}
			}
		}
	}()
}

// emit queues one event. It never blocks and never fails an update. The
// time is stamped here, when the thing happened: a slow server must not be
// able to move a timestamp minutes later.
func (u *Updater) emit(ev control.Event) {
	if u.Control == nil || u.evs == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	select {
	case u.evs <- ev:
	default:
		log.Printf("control events are backed up; dropped a %s event", ev.Type)
	}
}

// Round checks every watched container once. dryRun only reports.
func (u *Updater) Round(ctx context.Context, dryRun bool) ([]string, error) {
	return u.round(ctx, dryRun, "")
}

// round is Round. cmdID is set when a control-server command asked for it,
// so the events say which command they belong to.
func (u *Updater) round(ctx context.Context, dryRun bool, cmdID string) ([]string, error) {
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
					u.emit(registryEvent(w, err, cmdID))
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
				u.emit(availableEvent(w, d, cmdID))
			}
		case dryRun:
			add("%s: would update %s", w.Name, w.Ref)
		default:
			auth, err := u.Reg.Auth(w.Ref)
			if err != nil {
				u.Notify(add("%s: could not read the registry login: %v", w.Name, err))
				u.emit(failedEvent(w, d, err, cmdID))
				continue
			}
			res, err := u.Gate.Update(ctx, w.Name, d, auth)
			if err != nil {
				u.Notify(add("%s: update failed: %v", w.Name, err))
				u.emit(failedEvent(w, d, err, cmdID))
				continue
			}
			u.Notify(add("%s", res.Message))
			u.emit(updateEvent(w, d, res, cmdID))
		}
	}
	if !dryRun {
		// The gate's queued events are its own news, from hours earlier at
		// times; no command caused them, so they carry no command id.
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
		u.emit(gateEvent(e, ""))
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
		lines, err := u.round(ctx, false, "")
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
