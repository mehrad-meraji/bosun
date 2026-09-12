package updater

import (
	"errors"
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

var w1 = gate.Watched{Name: "redis", Ref: "redis:7.4", Digests: []string{"sha256:old"}, Mode: "update"}

func TestUpdateEventDone(t *testing.T) {
	res := gate.Result{
		Status: gate.StatusDone, Downtime: 38 * time.Second,
		Backup: 2 * time.Second, BackupBytes: 1024,
		Steps: []gate.Step{{Name: "pull", Status: "ok"}},
	}
	ev := updateEvent(w1, "sha256:new", res, "")
	if ev.Type != control.EventUpdateDone || ev.Container != "redis" || ev.Image != "redis:7.4" {
		t.Errorf("event = %+v", ev)
	}
	if ev.FromDigest != "sha256:old" || ev.ToDigest != "sha256:new" || ev.DowntimeMs != 38000 {
		t.Errorf("digests/downtime = %+v", ev)
	}
	if ev.Backup == nil || ev.Backup.Bytes != 1024 || ev.Backup.Ms != 2000 {
		t.Errorf("backup = %+v", ev.Backup)
	}
	if len(ev.Steps) != 1 || ev.CommandID != "" {
		t.Errorf("steps/command = %+v", ev)
	}
}

func TestUpdateEventRolledBack(t *testing.T) {
	ev := updateEvent(w1, "sha256:new", gate.Result{Status: gate.StatusReverted}, "cmd-1")
	if ev.Type != control.EventUpdateRolledBack || ev.Reason == "" || ev.CommandID != "cmd-1" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Backup != nil {
		t.Errorf("backup must be absent when there was none: %+v", ev.Backup)
	}
}

func TestFailedEventAndRefusedEvent(t *testing.T) {
	ev := failedEvent(w1, "sha256:new", errors.New("pull: no such tag"), "")
	if ev.Type != control.EventUpdateFailed || ev.Reason != "pull: no such tag" {
		t.Errorf("event = %+v", ev)
	}
	if len(ev.Steps) != 0 {
		t.Errorf("update.failed carries no steps; the gate sends steps only with a result: %+v", ev.Steps)
	}
	ev = failedEvent(w1, "sha256:new", &gate.RefusedError{Msg: "redis is notify-only"}, "")
	if ev.Type != control.EventGateRefused {
		t.Errorf("a gate refusal is its own event type: %+v", ev)
	}
}

func TestAvailableAndRegistryEvents(t *testing.T) {
	ev := availableEvent(w1, "sha256:new", "")
	if ev.Type != control.EventVersionAvailable || ev.ToDigest != "sha256:new" {
		t.Errorf("event = %+v", ev)
	}
	ev = registryEvent(w1, errors.New("429 too many requests"), "")
	if ev.Type != control.EventRegistryFailing || ev.Reason == "" || ev.Container != "redis" {
		t.Errorf("event = %+v", ev)
	}
}

func TestGateEvent(t *testing.T) {
	when := time.Date(2026, 9, 11, 4, 0, 0, 0, time.UTC)
	ev := gateEvent(state.Event{Time: when, Kind: "recovered", Name: "app", Message: "app: the old version is back"}, "")
	if ev.Type != control.EventRecovery || !ev.Time.Equal(when) || ev.Container != "app" || ev.Reason == "" {
		t.Errorf("event = %+v", ev)
	}
	ev = gateEvent(state.Event{Kind: "something-new", Name: "app", Message: "hm"}, "")
	if ev.Type != control.EventWarning {
		t.Errorf("an unknown gate kind is a warning: %+v", ev)
	}
}

func TestResultEvent(t *testing.T) {
	ev := resultEvent("cmd-1", "busy", "a round is running")
	if ev.Type != control.EventCommandResult || ev.CommandID != "cmd-1" || ev.Status != "busy" || ev.Reason == "" {
		t.Errorf("event = %+v", ev)
	}
}

// A rolled-back update's event carries the dead container's last output when
// the gate read it, and nothing when it did not.
func TestUpdateEventCarriesTheFailedLogs(t *testing.T) {
	res := gate.Result{Status: gate.StatusReverted, Logs: "boom: no such table"}
	if ev := updateEvent(w1, "sha256:new", res, ""); ev.Logs != "boom: no such table" {
		t.Errorf("Logs = %q, want the tail", ev.Logs)
	}
	if ev := updateEvent(w1, "sha256:new", gate.Result{Status: gate.StatusDone}, ""); ev.Logs != "" {
		t.Errorf("Logs = %q, want none", ev.Logs)
	}
}

// A recovery event carries the tail the gate parked in its state file.
func TestGateEventCarriesTheFailedLogs(t *testing.T) {
	e := state.Event{Kind: "recovered", Name: "app", Message: "app: the old version is back", Logs: "boom: migration failed"}
	if ev := gateEvent(e, ""); ev.Logs != "boom: migration failed" {
		t.Errorf("Logs = %q, want the tail", ev.Logs)
	}
}
