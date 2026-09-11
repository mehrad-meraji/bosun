// Package gate is the only part of Bosun that talks to Docker. It has no
// network. It offers a few fixed actions and builds every Docker request
// itself, so a caller can never add mounts, privileges or devices.
package gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/recreate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const (
	LabelEnable  = "bosun.enable"
	LabelMode    = "bosun.mode"
	LabelTimeout = "bosun.health-timeout"
	LabelManaged = "bosun.managed-by"
	UpdaterName  = "bosun-updater"

	StatusDone     = "done"
	StatusReverted = "reverted"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type Gate struct {
	D      *docker.Client
	Dir    string        // shared run folder, /run/bosun
	SelfID string        // the gate's own container ID (or its prefix); never touched
	Poll   time.Duration // health poll interval; 0 means 1s
}

// Watched is a container Bosun looks after.
type Watched struct {
	Name    string   `json:"name"`
	Ref     string   `json:"ref"`
	Digests []string `json:"digests"`
	Mode    string   `json:"mode"` // "update" or "notify"
	Skip    []string `json:"skip"`
}

type Result struct {
	Status   string        `json:"status"` // StatusDone or StatusReverted
	Downtime time.Duration `json:"downtime"`
	Message  string        `json:"message"`
}

// RefusedError is a request the gate will not do. It means a bug or an attack.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return "refused: " + e.Msg }

func refuse(format string, a ...any) error { return &RefusedError{Msg: fmt.Sprintf(format, a...)} }

// List returns the running containers with bosun.enable=true that follow a tag.
func (g *Gate) List(ctx context.Context) ([]Watched, error) {
	sums, err := g.D.List(ctx, LabelEnable+"=true", false)
	if err != nil {
		return nil, err
	}
	st, err := state.Read(g.Dir)
	if err != nil {
		return nil, err
	}
	out := []Watched{}
	for _, s := range sums {
		c, err := g.D.Inspect(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		ref, ok := trackable(c)
		if !ok || g.isSelf(c) {
			continue
		}
		img, err := g.D.InspectImage(ctx, c.Image)
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(c.Name, "/")
		mode := c.Config.Labels[LabelMode]
		if mode != "notify" {
			mode = "update"
		}
		out = append(out, Watched{Name: name, Ref: ref, Digests: digestsOf(img), Mode: mode, Skip: st.Entry(name).Skip})
	}
	return out, nil
}

// Update pulls the container's own tag, checks it is digest, and swaps the
// container onto it. The image ref always comes from the container, never
// from the caller.
func (g *Gate) Update(ctx context.Context, name, digest, auth string) (Result, error) {
	c, ref, err := g.watched(ctx, name)
	if err != nil {
		return Result{}, err
	}
	if c.Config.Labels[LabelMode] == "notify" {
		return Result{}, refuse("%s is notify-only", name)
	}
	f, st, err := state.Open(g.Dir, true)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	e := st.Entry(name)
	if slices.Contains(e.Skip, digest) {
		return Result{}, refuse("%s: %s is on the skip list", name, digest)
	}
	if err := g.D.Pull(ctx, ref, auth); err != nil {
		return Result{}, err
	}
	img, err := g.D.InspectImage(ctx, ref)
	if err != nil {
		return Result{}, err
	}
	if !slices.Contains(digestsOf(img), digest) {
		return Result{}, fmt.Errorf("%s: pulled %s but it is not %s (the tag moved?); skipped this round", name, ref, digest)
	}
	// From here on, finish even if the caller hangs up. A half-done swap is worse.
	ctx = context.WithoutCancel(ctx)
	res, err := g.swap(ctx, f, st, c, ref)
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		e.AddSkip(digest)
		return res, f.Save(st)
	}
	prev, err := g.keep(ctx, name, c.Image, e.Prev)
	if err != nil {
		log.Printf("%s: could not keep the old image for rollback: %v", name, err)
	}
	e.Prev, e.UpdatedAt, e.Downtime = prev, time.Now().UTC(), res.Downtime
	return res, f.Save(st)
}

// Rollback puts back the version kept by the last update.
func (g *Gate) Rollback(ctx context.Context, name string) (Result, error) {
	f, st, err := state.Open(g.Dir, false)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	e := st.Entry(name)
	if e.Prev == "" {
		return Result{}, fmt.Errorf("no old version kept for %s. Run `bosun rollback ls` to see what you can roll back", name)
	}
	c, ref, err := g.watched(ctx, name)
	if err != nil {
		return Result{}, err
	}
	cur, err := g.D.InspectImage(ctx, c.Image)
	if err != nil {
		return Result{}, err
	}
	repo, tag := docker.SplitRef(ref)
	// Point the tag back at the old image, so the container keeps a readable ref.
	if err := g.D.Tag(ctx, e.Prev, repo, tag); err != nil {
		return Result{}, fmt.Errorf("retag %s: %w", e.Prev, err)
	}
	ctx = context.WithoutCancel(ctx)
	res, err := g.swap(ctx, f, st, c, ref)
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		_ = g.D.Tag(ctx, c.Image, repo, tag)
		res.Message = fmt.Sprintf("%s: the old version did not come up healthy; the current version is kept", name)
		return res, nil
	}
	for _, d := range digestsOf(cur) {
		e.AddSkip(d)
	}
	_ = g.D.RemoveImage(ctx, e.Prev)
	e.Prev, e.UpdatedAt = "", time.Now().UTC()
	res.Message = fmt.Sprintf("%s: rolled back (down %s). The newer version is on the skip list; `bosun skip clear %s` allows it again",
		name, res.Downtime.Round(100*time.Millisecond), name)
	return res, f.Save(st)
}

// swap replaces old with a new container running ref, then waits for it to
// be healthy. If anything fails after the old one stops, it puts the old one
// back and returns StatusReverted.
func (g *Gate) swap(ctx context.Context, f *state.File, st *state.State, old *docker.Container, ref string) (Result, error) {
	name := strings.TrimPrefix(old.Name, "/")
	img, err := g.D.InspectImage(ctx, old.Image)
	if err != nil {
		return Result{}, err
	}
	body, err := recreate.Build(old.Raw, img.Config, ref)
	if err != nil {
		return Result{}, fmt.Errorf("%s: build the new container: %w", name, err)
	}

	p := state.Pending{Name: name, OldID: old.ID, TmpName: name + "-bosun-" + randHex()}
	st.Pending = append(st.Pending, p)
	if err := f.Save(st); err != nil {
		return Result{}, err
	}

	keepPending := false
	defer func() {
		if !keepPending {
			st.Pending = slices.DeleteFunc(st.Pending, func(q state.Pending) bool { return q == p })
			if err := f.Save(st); err != nil {
				log.Printf("save state: %v", err)
			}
		}
	}()

	t0 := time.Now()
	if err := g.D.Stop(ctx, old.ID); err != nil {
		return Result{}, fmt.Errorf("%s: stop: %w", name, err)
	}
	if err := g.D.Rename(ctx, old.ID, p.TmpName); err != nil {
		_ = g.D.Start(ctx, old.ID)
		return Result{}, fmt.Errorf("%s: rename: %w", name, err)
	}
	newID, err := g.D.Create(ctx, name, body)
	if err == nil {
		err = g.D.Start(ctx, newID)
	}
	down := time.Since(t0)
	if err == nil {
		err = g.waitHealthy(ctx, newID, timeoutOf(old))
	}
	if err != nil {
		if rerr := g.revert(ctx, old.ID, newID, name); rerr != nil {
			// revert failed; keep Pending in state so Recover can fix it on next gate start
			keepPending = true
			return Result{}, fmt.Errorf("%s: new version failed (%v) and putting the old one back failed: %w. The old container is stopped as %s; restart bosun-gate to recover it, or rename it back to %s and start it", name, err, rerr, p.TmpName, name)
		}
		return Result{Status: StatusReverted, Downtime: time.Since(t0),
			Message: fmt.Sprintf("%s: new version failed (%v); the old version is back", name, err)}, nil
	}
	if err := g.D.Remove(ctx, old.ID, true); err != nil {
		log.Printf("%s: remove old container %s: %v", name, p.TmpName, err)
	}
	return Result{Status: StatusDone, Downtime: down,
		Message: fmt.Sprintf("%s: now running %s (down %s)", name, ref, down.Round(100*time.Millisecond))}, nil
}

func (g *Gate) revert(ctx context.Context, oldID, newID, name string) error {
	if newID != "" {
		if err := g.D.Remove(ctx, newID, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	if err := g.D.Rename(ctx, oldID, name); err != nil {
		return err
	}
	return g.D.Start(ctx, oldID)
}

func (g *Gate) waitHealthy(ctx context.Context, id string, timeout time.Duration) error {
	poll := g.Poll
	if poll == 0 {
		poll = time.Second
	}
	t0 := time.Now()
	for {
		c, err := g.D.Inspect(ctx, id)
		if err != nil {
			return err
		}
		if done, err := verdict(c, time.Since(t0), timeout); done {
			return err
		}
		time.Sleep(poll)
	}
}

// verdict judges one look at the new container. done=false means keep
// waiting. With a HEALTHCHECK it must report healthy in time. Without one it
// must keep running, with no restarts, for the whole timeout.
func verdict(c *docker.Container, elapsed, timeout time.Duration) (done bool, err error) {
	if !c.State.Running || c.State.Restarting || c.RestartCount > 0 {
		return true, fmt.Errorf("container stopped (%s)", c.State.Status)
	}
	if h := c.State.Health; h != nil {
		switch h.Status {
		case "healthy":
			return true, nil
		case "unhealthy":
			return true, errors.New("healthcheck says unhealthy")
		}
		if elapsed >= timeout {
			return true, fmt.Errorf("not healthy after %s", timeout)
		}
		return false, nil
	}
	return elapsed >= timeout, nil
}

// keep tags the old image as bosun/prev/<name>:<id> so `docker image prune`
// leaves it for rollback, and drops the tag of the version kept before it.
func (g *Gate) keep(ctx context.Context, name, imageID, before string) (string, error) {
	repo := "bosun/prev/" + strings.ToLower(name)
	tag := strings.TrimPrefix(imageID, "sha256:")
	if len(tag) > 12 {
		tag = tag[:12]
	}
	if err := g.D.Tag(ctx, imageID, repo, tag); err != nil {
		return "", err
	}
	if before != "" && before != repo+":"+tag {
		_ = g.D.RemoveImage(ctx, before)
	}
	return repo + ":" + tag, nil
}

// watched loads a container the gate may change, or refuses.
func (g *Gate) watched(ctx context.Context, name string) (*docker.Container, string, error) {
	if !nameRE.MatchString(name) {
		return nil, "", refuse("bad container name %q", name)
	}
	c, err := g.D.Inspect(ctx, name)
	if docker.IsNotFound(err) {
		return nil, "", refuse("no container named %q", name)
	}
	if err != nil {
		return nil, "", err
	}
	if strings.TrimPrefix(c.Name, "/") != name {
		return nil, "", refuse("%q is not a container name; use the name, not the ID", name)
	}
	if c.Config.Labels[LabelEnable] != "true" {
		return nil, "", refuse("%s does not have %s=true", name, LabelEnable)
	}
	if g.isSelf(c) {
		return nil, "", refuse("%s is part of bosun", name)
	}
	ref, ok := trackable(c)
	if !ok {
		return nil, "", refuse("%s runs %q, which is pinned or not a tag", name, c.Config.Image)
	}
	return c, ref, nil
}

func (g *Gate) isSelf(c *docker.Container) bool {
	return (g.SelfID != "" && strings.HasPrefix(c.ID, g.SelfID)) || c.Config.Labels[LabelManaged] != ""
}

// trackable returns the tag a container follows. Images run by ID or pinned
// by digest are never updated.
func trackable(c *docker.Container) (string, bool) {
	ref := c.Config.Image
	if ref == "" || strings.HasPrefix(ref, "sha256:") || strings.Contains(ref, "@") {
		return "", false
	}
	return ref, true
}

func digestsOf(img *docker.Image) []string {
	var out []string
	for _, rd := range img.RepoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok {
			out = append(out, d)
		}
	}
	return out
}

func timeoutOf(c *docker.Container) time.Duration {
	if d, err := time.ParseDuration(c.Config.Labels[LabelTimeout]); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

func randHex() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
