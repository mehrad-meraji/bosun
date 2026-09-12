package gate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// Recover finishes swaps that were cut off by a crash or power loss. A new
// container that runs and passes the same health wait as an update is kept.
// Otherwise the old one comes back and the new version goes on the skip
// list. Each case leaves an event for the updater to send as a note.
func (g *Gate) Recover(ctx context.Context) error {
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return err
	}
	defer f.Close()
	// Keep only the records whose recovery failed; remove successful ones
	var failed []state.Pending
	for _, p := range st.Pending {
		msg, err := g.recoverOne(ctx, st, p)
		if err != nil {
			if p.KeepStopped {
				msg = fmt.Sprintf("%s: recovery of a rollback with data failed: %v. The old container may be stopped as %s; do not start it on this data. Rename it back to %s and run `bosun rollback %s --with-data` again.", p.Name, err, p.TmpName, p.Name, p.Name)
			} else {
				msg = fmt.Sprintf("%s: crash recovery failed: %v. The old container may be stopped as %s: rename it back to %s and start it, then restart bosun-gate.", p.Name, err, p.TmpName, p.Name)
			}
			failed = append(failed, p)
		}
		st.AddEvent("recovered", p.Name, msg)
	}
	st.Pending = failed
	return f.Save(st)
}

func (g *Gate) recoverOne(ctx context.Context, st *state.State, p state.Pending) (string, error) {
	// Check if the old container still exists
	old, oldErr := g.D.Inspect(ctx, p.OldID)
	if oldErr != nil && !docker.IsNotFound(oldErr) {
		return "", oldErr
	}
	if docker.IsNotFound(oldErr) {
		// Old container is gone. Check what's at p.Name now.
		cur, err := g.D.Inspect(ctx, p.Name)
		if err != nil && !docker.IsNotFound(err) {
			return "", err
		}
		if docker.IsNotFound(err) {
			// Neither old nor new container exists - this is a problem
			return "", fmt.Errorf("both old container %s and current container %s are missing", p.OldID, p.Name)
		}
		// A container is running at p.Name; keep it and start it if stopped,
		// unless its mounts hold restored backup data that must never run.
		if !cur.State.Running {
			if p.KeepStopped {
				return p.Name + ": an update was cut off; the old container was already gone, and the current one is stopped", nil
			}
			return p.Name + ": an update was cut off; the old container was already gone, so the current one was kept", g.D.Start(ctx, cur.ID)
		}
		return p.Name + ": an update was cut off; the old container was already gone, so the current one was kept", nil
	}

	// Old container exists. Check the container at p.Name
	cur, err := g.D.Inspect(ctx, p.Name)
	switch {
	case err != nil && !docker.IsNotFound(err):
		return "", err
	case err == nil && cur.ID == p.OldID:
		// Stopped or crashed before the rename. Nothing changed but the stop
		// and the tag, which the pull or rollback moved.
		if p.KeepStopped {
			return fmt.Sprintf("%s: a rollback with data was cut off; the data is from the backup, and %s is stopped. Run `bosun rollback %s --with-data` again", p.Name, p.Name, p.Name), nil
		}
		g.retag(ctx, old.Image, old.Config.Image)
		return p.Name + ": an update was cut off before it changed anything; the container is running again", g.D.Start(ctx, p.OldID)
	case err == nil && cur.State.Running && g.waitHealthy(ctx, cur.ID, timeoutOf(cur)) == nil:
		if err := g.D.Remove(ctx, p.OldID, true); err != nil && !docker.IsNotFound(err) {
			return "", err
		}
		g.kept(ctx, st.Entry(p.Name), p, old.Image)
		return p.Name + ": an update was cut off; the new version is running and was kept", nil
	case err == nil:
		if err := g.D.Remove(ctx, cur.ID, true); err != nil {
			return "", err
		}
	}
	if err := g.D.Rename(ctx, p.OldID, p.Name); err != nil {
		return "", err
	}
	if p.Digest != "" {
		st.Entry(p.Name).AddSkip(p.Digest)
	}
	if p.KeepStopped {
		// The volumes hold the restored (old) data, so the tag must stay on
		// the old image; do not point it back at the newer one.
		return fmt.Sprintf("%s: a rollback with data was cut off; %s is stopped with the backup's data. Run `bosun rollback %s --with-data` again", p.Name, p.Name, p.Name), nil
	}
	g.retag(ctx, old.Image, old.Config.Image)
	return p.Name + ": an update was cut off; the old version is back", g.D.Start(ctx, p.OldID)
}

// kept does the bookkeeping for a cut-off swap whose new container stays,
// as Update or Rollback would have done.
func (g *Gate) kept(ctx context.Context, e *state.Entry, p state.Pending, oldImage string) {
	if p.Digest == "" {
		// A rollback: skip the version it left, and nothing is kept to roll back to.
		if img, err := g.D.InspectImage(ctx, oldImage); err == nil {
			for _, d := range digestsOf(img) {
				e.AddSkip(d)
			}
		}
		if e.Prev != "" {
			_ = g.D.RemoveImage(ctx, e.Prev)
		}
		// The old version is running and healthy, so a --with-data rollback
		// is finished: clear the flag, or nothing can touch this app again.
		e.Prev, e.UpdatedAt, e.DataRestored = "", time.Now().UTC(), false
		return
	}
	prev, err := g.keep(ctx, p.Name, oldImage, e.Prev)
	if err != nil {
		log.Printf("%s: could not keep the old image for rollback: %v", p.Name, err)
	}
	e.Prev, e.UpdatedAt = prev, time.Now().UTC()
}

// SpawnUpdater starts Bosun's updater container. It clears any old one first.
func (g *Gate) SpawnUpdater(ctx context.Context) error {
	self, err := g.D.Inspect(ctx, g.SelfID)
	if err != nil {
		return fmt.Errorf("find own container %q (do not set hostname on bosun-gate): %w", g.SelfID, err)
	}
	if err := g.RemoveUpdaters(ctx); err != nil {
		return err
	}
	// The host name labels every control-server event. Docker knows it; the
	// user can override it with BOSUN_HOST on the gate.
	host := ""
	if !hasEnv(self.Config.Env, "BOSUN_HOST") {
		if h, err := g.D.Info(ctx); err != nil {
			log.Printf("read the Docker host name: %v; events will have no host", err)
		} else {
			host = h
		}
	}
	id, err := g.D.Create(ctx, UpdaterName, updaterBody(self, g.RunDir, host))
	if err != nil {
		return err
	}
	return g.D.Start(ctx, id)
}

// updaterBody is the updater's fixed container settings: the gate's image by
// ID (so a moved tag cannot swap it), no Docker socket, read-only, no
// capabilities. It gets the shared run folder and read-only copies of the
// gate's /etc/bosun mounts, and nothing else.
func updaterBody(self *docker.Container, dir, host string) map[string]any {
	binds := []string{}
	for _, m := range self.Mounts {
		src := m.Source
		if m.Type == "volume" {
			src = m.Name
		}
		switch {
		case strings.Contains(m.Source, "docker.sock"):
			continue // never, whatever the mount point
		case m.Destination == dir:
			binds = append(binds, src+":"+dir)
		case m.Destination == "/etc/bosun" || strings.HasPrefix(m.Destination, "/etc/bosun/"):
			binds = append(binds, src+":"+m.Destination+":ro")
		}
	}
	env := []string{"DOCKER_CONFIG=/etc/bosun/docker"}
	for _, e := range self.Config.Env {
		if strings.HasPrefix(e, "BOSUN_") {
			env = append(env, e)
		}
	}
	if host != "" {
		env = append(env, "BOSUN_HOST="+host)
	}
	return map[string]any{
		"Image":  self.Image,
		"User":   "65532:65532", // distroless nonroot, the owner of gate.sock
		"Cmd":    []string{"updater"},
		"Env":    env,
		"Labels": map[string]string{LabelManaged: "bosun-gate"},
		"HostConfig": map[string]any{
			"Binds":          binds,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"RestartPolicy":  map[string]string{"Name": "unless-stopped"},
			"NetworkMode":    "bridge",
		},
	}
}

// hasEnv reports whether the list already sets key.
func hasEnv(list []string, key string) bool {
	for _, e := range list {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// RemoveUpdaters removes every container the gate made.
func (g *Gate) RemoveUpdaters(ctx context.Context) error {
	sums, err := g.D.List(ctx, LabelManaged+"=bosun-gate", true)
	if err != nil {
		return err
	}
	for _, s := range sums {
		if err := g.D.Remove(ctx, s.ID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	return nil
}
