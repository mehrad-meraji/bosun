package updater

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
)

// Commands asks the control server for work every poll until ctx ends. The
// server never connects to Bosun, so a command runs up to one poll late.
// That is the price of having no open port.
func (u *Updater) Commands(ctx context.Context, poll time.Duration) error {
	if u.Control == nil {
		return nil
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		cmds, refs, err := u.Control.Commands(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("read commands: %v", err) // try again at the next poll
			}
			continue
		}
		u.runReply(ctx, cmds, refs)
	}
}

// runReply runs one poll's worth of commands and reports each one. The
// server already marked the refusals as terminal; each command is marked
// once its own result is terminal, so a busy one comes back next poll.
func (u *Updater) runReply(ctx context.Context, cmds []control.Command, refs []control.Refusal) {
	for _, r := range refs {
		log.Printf("refused command %s: %s", r.ID, r.Reason)
		u.emit(resultEvent(r.ID, "refused", r.Reason))
	}
	// Every check in one reply asks for the same thing: a round now. Running
	// all of them would let one reply start up to maxCommands rounds in a
	// row, hitting registry rate limits and pushing the scheduled round out.
	// So the first check runs and the rest report its result.
	var ranCheck bool
	var checkStatus, checkDetail string
	for _, c := range cmds {
		status, detail := "", ""
		switch {
		case c.Type == control.CmdCheck && ranCheck:
			status, detail = checkStatus, checkDetail
			log.Printf("command %s (check): the round this reply already asked for: %s", c.ID, status)
		case c.Type == control.CmdCheck:
			status, detail = u.runCommand(ctx, c)
			ranCheck, checkStatus, checkDetail = true, status, detail
			log.Printf("command %s (%s): %s: %s", c.ID, c.Type, status, detail)
		default:
			status, detail = u.runCommand(ctx, c)
			log.Printf("command %s (%s): %s: %s", c.ID, c.Type, status, detail)
		}
		if status != "busy" {
			u.Control.Done(c.ID) // terminal; a replay must not run it again
		}
		u.emit(resultEvent(c.ID, status, detail))
	}
}

// runCommand does one command and says how it went: done, busy, refused or
// failed. The server uses that to take the command off its list.
func (u *Updater) runCommand(ctx context.Context, c control.Command) (string, string) {
	switch c.Type {
	case control.CmdCheck:
		lines, err := u.round(ctx, false, c.ID)
		switch {
		case errors.Is(err, ErrBusy):
			return "busy", err.Error()
		case err != nil:
			return "failed", err.Error()
		}
		return "done", fmt.Sprintf("checked %d containers", len(lines))
	case control.CmdSkipClear:
		res, err := u.Gate.SkipClear(ctx, c.Container)
		switch {
		case gate.IsBusy(err):
			return "busy", err.Error()
		case gate.IsRefused(err):
			return "refused", err.Error()
		case err != nil:
			return "failed", err.Error()
		}
		return "done", res.Message
	}
	return "refused", "unknown command " + c.Type
}
