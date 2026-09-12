package updater

import (
	"github.com/mehrad-meraji/bosun/internal/control"
	"github.com/mehrad-meraji/bosun/internal/gate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// first is the digest a container runs now, or "" if Docker gave none.
func first(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return ds[0]
}

// updateEvent reports a finished update, done or rolled back.
func updateEvent(w gate.Watched, digest string, res gate.Result, cmdID string) control.Event {
	ev := control.Event{
		Type:       control.EventUpdateDone,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		DowntimeMs: res.Downtime.Milliseconds(),
		Steps:      res.Steps,
		CommandID:  cmdID,
	}
	if res.Status == gate.StatusReverted {
		ev.Type = control.EventUpdateRolledBack
		ev.Reason = "the new version did not come up healthy"
	}
	if res.BackupBytes > 0 {
		ev.Backup = &control.Backup{Bytes: res.BackupBytes, Ms: res.Backup.Milliseconds()}
	}
	return ev
}

// failedEvent reports an update that did not finish. The gate sends steps
// only with a result, so there are none here; the reason says what failed.
func failedEvent(w gate.Watched, digest string, err error, cmdID string) control.Event {
	t := control.EventUpdateFailed
	if gate.IsRefused(err) {
		t = control.EventGateRefused
	}
	return control.Event{
		Type:       t,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		Reason:     err.Error(),
		CommandID:  cmdID,
	}
}

// availableEvent reports a new version for a notify-only container.
func availableEvent(w gate.Watched, digest, cmdID string) control.Event {
	return control.Event{
		Type:       control.EventVersionAvailable,
		Container:  w.Name,
		Image:      w.Ref,
		FromDigest: first(w.Digests),
		ToDigest:   digest,
		CommandID:  cmdID,
	}
}

// registryEvent reports a registry that keeps failing.
func registryEvent(w gate.Watched, err error, cmdID string) control.Event {
	return control.Event{
		Type:      control.EventRegistryFailing,
		Container: w.Name,
		Image:     w.Ref,
		Reason:    err.Error(),
		CommandID: cmdID,
	}
}

// gateEvent passes on something that happened in the gate while no call was
// open, such as crash recovery.
func gateEvent(e state.Event, cmdID string) control.Event {
	t := control.EventWarning
	if e.Kind == "recovered" {
		t = control.EventRecovery
	}
	return control.Event{
		Type:      t,
		Time:      e.Time,
		Container: e.Name,
		Reason:    e.Message,
		CommandID: cmdID,
	}
}

// resultEvent tells the server what became of one of its commands, so it can
// take the command off its list.
func resultEvent(cmdID, status, detail string) control.Event {
	return control.Event{
		Type:      control.EventCommandResult,
		Status:    status,
		Reason:    detail,
		CommandID: cmdID,
	}
}
