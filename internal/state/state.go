// Package state is the gate's small state file, /var/lib/bosun/state.json, in
// a folder only the gate has.
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
	Digest  string `json:"digest,omitempty"` // the version an update goes to; empty for a rollback
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
