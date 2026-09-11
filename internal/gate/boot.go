package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// Recover finishes swaps that were cut off by a crash or power loss. A new
// container that runs and is healthy is kept. Otherwise the old one comes
// back. Each case leaves an event for the updater to send as a note.
func (g *Gate) Recover(ctx context.Context) error {
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range st.Pending {
		msg, err := g.recoverOne(ctx, p)
		if err != nil {
			msg = fmt.Sprintf("%s: crash recovery failed: %v. Check it by hand with `docker ps -a`", p.Name, err)
		}
		st.AddEvent("recovered", p.Name, msg)
	}
	st.Pending = nil
	return f.Save(st)
}

func (g *Gate) recoverOne(ctx context.Context, p state.Pending) (string, error) {
	cur, err := g.D.Inspect(ctx, p.Name)
	switch {
	case err != nil && !docker.IsNotFound(err):
		return "", err
	case err == nil && cur.ID == p.OldID:
		// Stopped or crashed before the rename. Nothing changed but the stop.
		return p.Name + ": an update was cut off before it changed anything; the container is running again", g.D.Start(ctx, p.OldID)
	case err == nil && cur.State.Running && (cur.State.Health == nil || cur.State.Health.Status == "healthy"):
		if err := g.D.Remove(ctx, p.OldID, true); err != nil && !docker.IsNotFound(err) {
			return "", err
		}
		return p.Name + ": an update was cut off; the new version is running and was kept", nil
	case err == nil:
		if err := g.D.Remove(ctx, cur.ID, true); err != nil {
			return "", err
		}
	}
	if err := g.D.Rename(ctx, p.OldID, p.Name); err != nil {
		return "", err
	}
	return p.Name + ": an update was cut off; the old version is back", g.D.Start(ctx, p.OldID)
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
	id, err := g.D.Create(ctx, UpdaterName, updaterBody(self, g.Dir))
	if err != nil {
		return err
	}
	return g.D.Start(ctx, id)
}

// updaterBody is the updater's fixed container settings: the gate's image by
// ID (so a moved tag cannot swap it), no Docker socket, read-only, no
// capabilities. It gets the shared run folder and read-only copies of the
// gate's /etc/bosun mounts, and nothing else.
func updaterBody(self *docker.Container, dir string) map[string]any {
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
	return map[string]any{
		"Image":  self.Image,
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
