package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// The only two commands there are. On purpose: a hacked server can make
// Bosun run an early round and retry a version it already refused. It
// cannot pick an image, roll back, or touch data.
const (
	CmdCheck     = "check"
	CmdSkipClear = "skip_clear"
)

// Command is one thing the server asks for.
type Command struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Container string `json:"container,omitempty"`
}

// Refusal is a command Bosun will not run, to report back.
type Refusal struct {
	ID     string
	Reason string
}

const (
	maxCommands = 100     // more than this in one reply is not a real list
	maxBody     = 64 << 10
	replayFor   = 24 * time.Hour
	maxSeen     = 10000 // ponytail: a flat cap; if a server ever floods unique IDs, the map is cleared and a replay costs at most one extra round
)

var (
	idRE   = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`) // same as the gate's; the gate checks again
)

// Commands asks the server what to do. It returns the commands to run and
// the ones to report as refused. A command already run in the last 24 hours
// is dropped without a word, so a replay cannot run it twice.
func (c *Client) Commands(ctx context.Context) ([]Command, []Refusal, error) {
	u := c.url + "/commands?" + url.Values{"host": {c.host}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("control server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, nil, &statusError{status: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("read the command list: %w", err)
	}
	cmds, refs, err := parse(b)
	if err != nil {
		return nil, nil, err
	}
	return c.fresh(cmds), refs, nil
}

// parse reads the command list. Unknown fields and unknown commands are
// refused, never guessed at.
func parse(b []byte) ([]Command, []Refusal, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var list []Command
	if err := dec.Decode(&list); err != nil {
		return nil, nil, fmt.Errorf("bad command list: %w", err)
	}
	if dec.More() {
		return nil, nil, fmt.Errorf("bad command list: more than one list")
	}
	if len(list) > maxCommands {
		return nil, nil, fmt.Errorf("bad command list: %d commands, more than the %d allowed", len(list), maxCommands)
	}
	var cmds []Command
	var refs []Refusal
	for _, cmd := range list {
		if !idRE.MatchString(cmd.ID) {
			// Without a usable id there is nothing to report it against.
			log.Printf("control server: dropped a command with a bad id")
			continue
		}
		switch {
		case cmd.Type == CmdCheck && cmd.Container == "":
			cmds = append(cmds, cmd)
		case cmd.Type == CmdCheck:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: "check takes no container"})
		case cmd.Type == CmdSkipClear && nameRE.MatchString(cmd.Container):
			cmds = append(cmds, cmd)
		case cmd.Type == CmdSkipClear:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: "skip_clear needs a container name"})
		default:
			refs = append(refs, Refusal{ID: cmd.ID, Reason: fmt.Sprintf("unknown command %q", cmd.Type)})
		}
	}
	return cmds, refs, nil
}

// fresh drops commands already run in the last 24 hours.
func (c *Client) fresh(in []Command) []Command {
	now := time.Now()
	for id, t := range c.seen {
		if now.Sub(t) > replayFor {
			delete(c.seen, id)
		}
	}
	if len(c.seen) > maxSeen {
		log.Printf("control server: over %d command ids in a day; forgetting them", maxSeen)
		c.seen = map[string]time.Time{}
	}
	var out []Command
	for _, cmd := range in {
		if _, ok := c.seen[cmd.ID]; ok {
			log.Printf("control server: command %s was already run; dropped", cmd.ID)
			continue
		}
		c.seen[cmd.ID] = now
		out = append(out, cmd)
	}
	return out
}
