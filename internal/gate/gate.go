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

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/recreate"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const (
	LabelEnable  = "bosun.enable"
	LabelMode    = "bosun.mode"
	LabelTimeout = "bosun.health-timeout"
	LabelManaged = "bosun.managed-by"
	LabelBackup  = "bosun.backup"
	UpdaterName  = "bosun-updater"

	StatusDone     = "done"
	StatusReverted = "reverted"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type Gate struct {
	D      *docker.Client
	Dir    string        // gate-only state folder, /var/lib/bosun; never given to the updater
	RunDir string        // shared socket folder, /run/bosun; the only folder the updater gets
	SelfID string        // the gate's own container ID (or its prefix); never touched
	Poll   time.Duration // health poll interval; 0 means 1s

	BackupDir   string // gate-only backup folder, /var/lib/bosun-backups; never given to the updater
	WarnSize    int64  // a backup bigger than this gets a one-time warning; 0 means never
	BackupSrc   string // BackupDir as Docker sees it (volume name or host path); found from the gate's mounts if empty
	HelperImage string // image for the restore helper; the gate's own image if empty
}

// Watched is a container Bosun looks after.
type Watched struct {
	Name    string   `json:"name"`
	Ref     string   `json:"ref"`
	Digests []string `json:"digests"`
	Mode    string   `json:"mode"` // "update" or "notify"
	Skip    []string `json:"skip"`
	Backup  bool     `json:"backup"`
}

type Result struct {
	Status      string        `json:"status"` // StatusDone or StatusReverted
	Downtime    time.Duration `json:"downtime"`
	Backup      time.Duration `json:"backup,omitempty"`       // time the backup took, inside Downtime
	BackupBytes int64         `json:"backup_bytes,omitempty"` // size of the backup
	Message     string        `json:"message"`
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
		if docker.IsNotFound(err) {
			continue // removed since the list call
		}
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
		out = append(out, Watched{Name: name, Ref: ref, Digests: digestsOf(img), Mode: mode, Skip: st.Entry(name).Skip, Backup: c.Config.Labels[LabelBackup] == "true"})
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
	if !c.State.Running {
		return Result{}, refuse("%s is not running; bosun only updates running containers", name)
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
	if img.ID == c.Image {
		return Result{Status: StatusDone, Message: fmt.Sprintf("%s: already running %s", name, ref)}, nil
	}
	// From here on, finish even if the caller hangs up. A half-done swap is worse.
	ctx = context.WithoutCancel(ctx)
	res, err := g.swap(ctx, f, st, c, ref, digest, c.Config.Labels[LabelBackup] == "true")
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		e.AddSkip(digest)
		g.retag(ctx, c.Image, ref)
		res.Message += fmt.Sprintf(". The new version is on the skip list; `bosun skip clear %s` allows it again", name)
		return res, f.Save(st)
	}
	prev, err := g.keep(ctx, name, c.Image, e.Prev)
	if err != nil {
		log.Printf("%s: could not keep the old image for rollback: %v", name, err)
	}
	e.Prev, e.UpdatedAt, e.Downtime = prev, time.Now().UTC(), res.Downtime
	if g.WarnSize > 0 && res.BackupBytes > g.WarnSize && !e.WarnedBig {
		res.Message += fmt.Sprintf(". Warning: the backup of %s is %s, so its updates have long downtime", name, backup.FormatSize(res.BackupBytes))
		e.WarnedBig = true
	}
	return res, f.Save(st)
}

// Rollback puts back the version kept by the last update. withData also puts
// the volumes back from the backup that belongs with that version.
func (g *Gate) Rollback(ctx context.Context, name string, withData bool) (Result, error) {
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
	var m *backup.Manifest
	if withData {
		if m, err = g.DataBackup(ctx, name, e.Prev); err != nil {
			return Result{}, err
		}
	}
	repo, tag := docker.SplitRef(ref)
	// Point the tag back at the old image, so the container keeps a readable ref.
	if err := g.D.Tag(ctx, e.Prev, repo, tag); err != nil {
		return Result{}, fmt.Errorf("retag %s: %w", e.Prev, err)
	}
	ctx = context.WithoutCancel(ctx)
	// Unless the rollback works, point the tag back at the current image.
	ok := false
	defer func() {
		if !ok {
			g.retag(ctx, c.Image, ref)
		}
	}()
	if withData {
		if err := g.D.Stop(ctx, c.ID); err != nil {
			return Result{}, fmt.Errorf("%s: stop: %w", name, err)
		}
		if err := g.restore(ctx, c, m); err != nil {
			if errors.Is(err, errUntouched) {
				if serr := g.D.Start(ctx, c.ID); serr != nil {
					return Result{}, fmt.Errorf("%s: %v. Starting it again failed: %v. Start it with: docker start %s", name, err, serr, name)
				}
				return Result{}, fmt.Errorf("%s: %v. The current version is running again", name, err)
			}
			return Result{}, fmt.Errorf("%s: %v. %s is stopped with incomplete data; run `bosun rollback %s --with-data` again", name, err, name, name)
		}
	}
	res, err := g.swap(ctx, f, st, c, ref, "", false)
	if err != nil {
		return Result{}, err
	}
	if res.Status == StatusReverted {
		if withData {
			// The revert started the newer version on the old data. Stop it.
			_ = g.D.Stop(ctx, c.ID)
			res.Message = fmt.Sprintf("%s: the data went back to the backup from %s, but the old version did not come up healthy. %s is stopped, so the newer version does not run on old data. Check `docker logs %s`",
				name, m.Time.Format("2006-01-02 15:04"), name, name)
			return res, nil
		}
		res.Message = fmt.Sprintf("%s: the old version did not come up healthy; the current version is kept", name)
		return res, nil
	}
	ok = true
	for _, d := range digestsOf(cur) {
		e.AddSkip(d)
	}
	_ = g.D.RemoveImage(ctx, e.Prev)
	e.Prev, e.UpdatedAt = "", time.Now().UTC()
	res.Message = fmt.Sprintf("%s: rolled back (down %s). The newer version is on the skip list; `bosun skip clear %s` allows it again",
		name, res.Downtime.Round(100*time.Millisecond), name)
	if withData {
		res.Message += fmt.Sprintf(". Data is from the backup of %s", m.Time.Format("2006-01-02 15:04"))
	}
	return res, f.Save(st)
}

// swap replaces old with a new container running ref, then waits for it to
// be healthy. If anything fails after the old one stops, it puts the old one
// back and returns StatusReverted. digest is the version an update goes to,
// or "" for a rollback; crash recovery reads it. withBackup copies the old
// container's mounts while it is stopped, before anything else changes.
func (g *Gate) swap(ctx context.Context, f *state.File, st *state.State, old *docker.Container, ref, digest string, withBackup bool) (Result, error) {
	name := strings.TrimPrefix(old.Name, "/")
	img, err := g.D.InspectImage(ctx, old.Image)
	if err != nil {
		return Result{}, err
	}
	body, err := recreate.Build(old.Raw, img.Config, ref)
	if err != nil {
		return Result{}, fmt.Errorf("%s: build the new container: %w", name, err)
	}

	p := state.Pending{Name: name, OldID: old.ID, TmpName: name + "-bosun-" + randHex(), Digest: digest}
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
	var took time.Duration
	var size int64
	if withBackup {
		b0 := time.Now()
		m, err := g.takeBackup(ctx, old)
		if err != nil {
			if serr := g.D.Start(ctx, old.ID); serr != nil {
				return Result{}, fmt.Errorf("%s: backup failed (%v), and restarting it failed: %v. %s is stopped. Start it with: docker start %s", name, err, serr, name, name)
			}
			return Result{}, fmt.Errorf("%s: backup failed, so no update: %w. The old version is running again", name, err)
		}
		took, size = time.Since(b0), m.Bytes()
	}
	if err := g.D.Rename(ctx, old.ID, p.TmpName); err != nil {
		if serr := g.D.Start(ctx, old.ID); serr != nil {
			return Result{}, fmt.Errorf("%s: rename: %v, and restarting it failed: %v. %s is stopped. Start it with: docker start %s", name, err, serr, name, name)
		}
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
		if renamed, rerr := g.revert(ctx, old.ID, newID, name); rerr != nil {
			// revert failed; keep Pending in state so Recover can fix it on next gate start
			keepPending = true
			if renamed {
				return Result{}, fmt.Errorf("%s: new version failed (%v) and starting the old one failed: %w. %s is stopped; start it with: docker start %s", name, err, rerr, name, name)
			}
			return Result{}, fmt.Errorf("%s: new version failed (%v) and putting the old one back failed: %w. The old container is stopped as %s; restart bosun-gate to recover it, or rename it back to %s and start it", name, err, rerr, p.TmpName, name)
		}
		return Result{Status: StatusReverted, Downtime: time.Since(t0),
			Message: fmt.Sprintf("%s: new version failed (%v); the old version is back", name, err)}, nil
	}
	if err := g.D.Remove(ctx, old.ID, true); err != nil {
		log.Printf("%s: remove old container %s: %v", name, p.TmpName, err)
	}
	msg := fmt.Sprintf("%s: now running %s (down %s)", name, ref, down.Round(100*time.Millisecond))
	if withBackup {
		msg = fmt.Sprintf("%s: now running %s (down %s, backup %s, %s)", name, ref,
			down.Round(100*time.Millisecond), took.Round(100*time.Millisecond), backup.FormatSize(size))
	}
	return Result{Status: StatusDone, Downtime: down, Backup: took, BackupBytes: size, Message: msg}, nil
}

// revert removes the new container and puts the old one back. renamed says
// the old one already has its name again, so only its start failed.
func (g *Gate) revert(ctx context.Context, oldID, newID, name string) (renamed bool, err error) {
	if newID != "" {
		if err := g.D.Remove(ctx, newID, true); err != nil && !docker.IsNotFound(err) {
			return false, err
		}
	}
	if err := g.D.Rename(ctx, oldID, name); err != nil {
		return false, err
	}
	return true, g.D.Start(ctx, oldID)
}

// retag points ref back at image, so the local tag matches what runs.
func (g *Gate) retag(ctx context.Context, image, ref string) {
	repo, tag := docker.SplitRef(ref)
	if err := g.D.Tag(ctx, image, repo, tag); err != nil {
		log.Printf("point %s back at %s: %v", ref, image, err)
	}
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
