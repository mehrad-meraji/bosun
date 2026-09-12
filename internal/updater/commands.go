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
		for _, r := range refs {
			log.Printf("refused command %s: %s", r.ID, r.Reason)
			u.emit(resultEvent(r.ID, "refused", r.Reason))
		}
		for _, c := range cmds {
			status, detail := u.runCommand(ctx, c)
			log.Printf("command %s (%s): %s: %s", c.ID, c.Type, status, detail)
			u.emit(resultEvent(c.ID, status, detail))
		}
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
