package updater

import (
	"context"
	"errors"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func TestRunCommandCheck(t *testing.T) {
	u, g, _ := setup(web("update", []string{"sha256:old"}, nil), fakeReg{digest: "sha256:new"})
	status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "done" || len(g.updates) != 1 {
		t.Errorf("status = %q, updates = %v, want done and one update", status, g.updates)
	}
}

func TestRunCommandCheckIsBusy(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{digest: "sha256:new"})
	u.mu.Lock() // a round is running
	defer u.mu.Unlock()
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdCheck})
	if status != "busy" || detail == "" {
		t.Errorf("status = %q, %q, want busy with a reason", status, detail)
	}
}

func TestRunCommandSkipClear(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.cleared = gate.SkipClearResult{Cleared: 2, Message: "app: skip list cleared (2)"}
	status, detail := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"})
	if status != "done" || detail == "" || g.skipCleared != "app" {
		t.Errorf("status = %q, %q, cleared %q, want done for app", status, detail, g.skipCleared)
	}
}

func TestRunCommandSkipClearBusyAndRefused(t *testing.T) {
	u, g, _ := setup(nil, fakeReg{})
	g.clearErr = state.ErrBusy // the gate's own error; over the socket it is a 409, which gate.IsBusy also reads
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: control.CmdSkipClear, Container: "app"}); status != "busy" {
		t.Errorf("status = %q, want busy", status)
	}
	g.clearErr = &gate.RefusedError{Msg: "bad container name"}
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c2", Type: control.CmdSkipClear, Container: "app"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
	g.clearErr = errors.New("disk on fire")
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c3", Type: control.CmdSkipClear, Container: "app"}); status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestRunCommandUnknownType(t *testing.T) {
	u, _, _ := setup(nil, fakeReg{})
	if status, _ := u.runCommand(context.Background(), control.Command{ID: "c1", Type: "reboot"}); status != "refused" {
		t.Errorf("status = %q, want refused", status)
	}
}
