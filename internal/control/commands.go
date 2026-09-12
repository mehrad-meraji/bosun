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
// the ones to report as refused. A command whose result was terminal in the
// last 24 hours is dropped without a word, so a replay cannot run it twice.
// A command still without a terminal result (a busy one) comes back, which
// is what "try again later" means.
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
	cmds = c.fresh(cmds) // also prunes seen, once per poll
	// A refusal is terminal: nothing ran, and nothing ever will. Report it
	// once and remember it, or every poll reports the same refusal forever.
	var fresh []Refusal
	for _, r := range refs {
		if _, ok := c.seen[r.ID]; ok {
			continue
		}
		fresh = append(fresh, r)
	}
	for _, r := range fresh {
		c.Done(r.ID)
	}
	return cmds, fresh, nil
}

// parse reads the command list. Unknown fields and unknown commands are
// refused, never guessed at. Each item is decoded on its own: one bad item
// is refused by its id, and the rest of the reply still runs. A whole-list
// problem (junk, not a list, two lists, over the cap) takes everything.
func parse(b []byte) ([]Command, []Refusal, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var list []json.RawMessage
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
	for _, raw := range list {
		cmd, err := parseOne(raw)
		if err != nil {
			// A field Bosun does not know: refuse this one item, so a server
			// that adds an optional field does not brick the whole reply.
			id := looseID(raw)
			if !idRE.MatchString(id) {
				log.Printf("control server: dropped a command that did not parse and has no usable id")
				continue
			}
			refs = append(refs, Refusal{ID: id, Reason: clip(err.Error())})
			continue
		}
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
			// The type is the server's bytes; echo only a little of it.
			refs = append(refs, Refusal{ID: cmd.ID, Reason: fmt.Sprintf("unknown command %q", clip(cmd.Type))})
		}
	}
	return cmds, refs, nil
}

// parseOne decodes one item of the list, strictly: a field Bosun does not
// know is an error, never a guess.
func parseOne(raw []byte) (Command, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cmd Command
	if err := dec.Decode(&cmd); err != nil {
		return Command{}, err
	}
	return cmd, nil
}

// looseID reads just the id of an item that failed the strict decode, so the
// failure can be reported against it. "" when there is none to read.
func looseID(raw []byte) string {
	var only struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &only); err != nil {
		return ""
	}
	return only.ID
}

// maxEcho is how many of the server's own bytes a refusal may repeat. A
// reply can be 64 KB; a reason is read by a person.
const maxEcho = 64

// clip shortens text that came from the server.
func clip(s string) string {
	if len(s) <= maxEcho {
		return s
	}
	return s[:maxEcho] + "..."
}

// Done remembers that this command reached a terminal result, so a replay
// cannot run it twice. A command with no terminal result yet (a busy one) is
// left unmarked on purpose: the server's next poll offers it again.
func (c *Client) Done(id string) { c.seen[id] = time.Now() }

// fresh drops commands whose result was already terminal in the last 24
// hours. It only reads seen; Done is what writes to it.
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
		out = append(out, cmd)
	}
	return out
}
