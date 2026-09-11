package gate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
)

var (
	tarFileRE    = regexp.MustCompile(`^[0-9]+\.tar$`)
	errUntouched = errors.New("nothing was changed")
)

// DataBackup returns name's backup if it belongs with the kept version prev.
func (g *Gate) DataBackup(ctx context.Context, name, prev string) (*backup.Manifest, error) {
	if g.BackupDir == "" {
		return nil, errors.New("no backup folder is set; set BOSUN_BACKUP_DIR")
	}
	m, err := backup.ReadManifest(filepath.Join(g.BackupDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no data backup for %s. Add the label bosun.backup=true so the next update makes one, or roll back without --with-data", name)
	}
	if err != nil {
		return nil, err
	}
	img, err := g.D.InspectImage(ctx, prev)
	if err != nil {
		return nil, err
	}
	if img.ID != m.Image {
		return nil, fmt.Errorf("the data backup of %s belongs to a different version than %s; roll back without --with-data", name, prev)
	}
	return m, nil
}

// restoreBody is the restore helper's fixed settings: Bosun's image, root
// with only the powers to delete and keep owners, no network, read-only
// root, the backup folder read-only, and only c's backed-up mounts.
func restoreBody(image, backupSrc, name string, m *backup.Manifest, c *docker.Container) (map[string]any, error) {
	binds := []string{backupSrc + ":/backup:ro"}
	args := []string{"restore-helper"}
	for i, bm := range m.Mounts {
		if !tarFileRE.MatchString(bm.File) {
			return nil, fmt.Errorf("bad file name %q in the backup of %s", bm.File, name)
		}
		mt, ok := mountAt(c, bm.Dest)
		if !ok || strings.Contains(mt.Source, "docker.sock") {
			return nil, fmt.Errorf("%s no longer has a writable mount at %s, so its backup cannot go back", name, bm.Dest)
		}
		src := mt.Source
		if mt.Type == "volume" {
			src = mt.Name
		}
		target := "/data/" + strconv.Itoa(i)
		binds = append(binds, src+":"+target)
		args = append(args, "/backup/"+name+"/"+bm.File+"="+target)
	}
	return map[string]any{
		"Image":  image,
		"User":   "0:0",
		"Cmd":    args,
		"Tty":    true, // plain logs, no framing
		"Labels": map[string]string{LabelManaged: "bosun-gate"},
		"HostConfig": map[string]any{
			"Binds":          binds,
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
	}, nil
}

func mountAt(c *docker.Container, dest string) (docker.Mount, bool) {
	for _, m := range backupMounts(c) {
		if m.Destination == dest {
			return m, true
		}
	}
	return docker.Mount{}, false
}

// helper finds the image to run the restore helper from (the gate's own, by
// ID) and the backup folder's source as Docker sees it.
func (g *Gate) helper(ctx context.Context) (image, src string, err error) {
	image, src = g.HelperImage, g.BackupSrc
	if image != "" && src != "" {
		return image, src, nil
	}
	self, err := g.D.Inspect(ctx, g.SelfID)
	if err != nil {
		return "", "", fmt.Errorf("find own container %q: %w", g.SelfID, err)
	}
	if image == "" {
		image = self.Image
	}
	for _, m := range self.Mounts {
		if src == "" && filepath.Clean(m.Destination) == filepath.Clean(g.BackupDir) {
			src = m.Source
			if m.Type == "volume" {
				src = m.Name
			}
		}
	}
	if src == "" {
		return "", "", fmt.Errorf("%s is not a mount of bosun-gate; add the bosun-backups volume (see README)", g.BackupDir)
	}
	return image, src, nil
}

// restore puts c's mounts back from backup m with the restore helper. c must
// be stopped. An error wrapping errUntouched means no data was changed.
func (g *Gate) restore(ctx context.Context, c *docker.Container, m *backup.Manifest) error {
	name := strings.TrimPrefix(c.Name, "/")
	image, src, err := g.helper(ctx)
	if err != nil {
		return fmt.Errorf("%w (%w)", err, errUntouched)
	}
	body, err := restoreBody(image, src, name, m, c)
	if err != nil {
		return fmt.Errorf("%w (%w)", err, errUntouched)
	}
	id, err := g.D.Create(ctx, name+"-bosun-restore", body)
	if err != nil {
		return fmt.Errorf("start the restore helper: %w (%w)", err, errUntouched)
	}
	defer g.D.Remove(context.WithoutCancel(ctx), id, true)
	if err := g.D.Start(ctx, id); err != nil {
		return fmt.Errorf("start the restore helper: %w (%w)", err, errUntouched)
	}
	code, err := g.D.Wait(ctx, id)
	if err != nil {
		return fmt.Errorf("restore helper: %w", err)
	}
	if code == 0 {
		return nil
	}
	out, _ := g.D.Logs(ctx, id)
	// 3 means the helper changed nothing. Any other code, 2 (a Go crash)
	// included, may have left the data half done.
	if code == 3 {
		return fmt.Errorf("restore helper refused: %s (%w)", out, errUntouched)
	}
	return fmt.Errorf("restore helper failed (exit %d): %s", code, out)
}
