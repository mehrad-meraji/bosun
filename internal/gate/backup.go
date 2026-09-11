package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
)

var errNoSpace = errors.New("not enough free space in the backup folder")

const noSpaceHint = "; free space in the backup folder, or remove bosun.backup=true"

// backupMounts are the mounts a backup copies: writable volumes and host
// folders. Read-only mounts cannot change, so they need no copy.
// ponytail: sockets are skipped by name (*.sock); Docker cannot tar them.
func backupMounts(c *docker.Container) []docker.Mount {
	var out []docker.Mount
	for _, m := range c.Mounts {
		if m.RW && (m.Type == "volume" || m.Type == "bind") && !strings.HasSuffix(m.Source, ".sock") {
			out = append(out, m)
		}
	}
	return out
}

// backupPaths are name's backup folder and the temp folders beside it.
// Hidden names (leading dot) so a container literally named "<name>.new"
// or "<name>.old" never collides with our temp/backup folders — Docker
// container names must start with a letter or digit, never a dot.
func (g *Gate) backupPaths(name string) (dir, tmp, old string) {
	return filepath.Join(g.BackupDir, name), filepath.Join(g.BackupDir, "."+name+".new"), filepath.Join(g.BackupDir, "."+name+".old")
}

// checkBackupSpace refuses a backup of name before the app is stopped when
// the backup folder is unset or clearly too full.
// ponytail: the last backup's size is the free-space guess; a disk that
// fills anyway is caught as ENOSPC during the copy.
func (g *Gate) checkBackupSpace(name string) error {
	if g.BackupDir == "" {
		return fmt.Errorf("%s has %s=true but no backup folder is set; set BOSUN_BACKUP_DIR", name, LabelBackup)
	}
	dir, _, _ := g.backupPaths(name)
	if prev, err := backup.ReadManifest(dir); err == nil {
		if free, err := backup.Free(g.BackupDir); err == nil && free < uint64(prev.Bytes()) {
			return fmt.Errorf("%w: the last backup of %s was %s and only %s is free"+noSpaceHint,
				errNoSpace, name, backup.FormatSize(prev.Bytes()), backup.FormatSize(int64(free)))
		}
	}
	return nil
}

// takeBackup copies c's mounts into BackupDir/.<name>.new using Docker's
// archive API. c must be stopped, so the files are in a clean state. commit
// swaps the new copy in for the old one; until then the old one is kept.
// A copy that is never committed must be removed by the caller.
func (g *Gate) takeBackup(ctx context.Context, c *docker.Container) (m *backup.Manifest, commit func() error, err error) {
	name := strings.TrimPrefix(c.Name, "/")
	if g.BackupDir == "" {
		return nil, nil, fmt.Errorf("%s has %s=true but no backup folder is set; set BOSUN_BACKUP_DIR", name, LabelBackup)
	}
	dir, tmp, old := g.backupPaths(name)
	if err := os.RemoveAll(tmp); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, nil, err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp)
		}
	}()
	m = &backup.Manifest{Image: c.Image, Time: time.Now().UTC()}
	for i, mt := range backupMounts(c) {
		file := strconv.Itoa(i) + ".tar"
		n, err := g.copyOut(ctx, c.ID, mt.Destination, filepath.Join(tmp, file))
		if err != nil {
			return nil, nil, fmt.Errorf("copy %s: %w", mt.Destination, err)
		}
		m.Mounts = append(m.Mounts, backup.Mount{Dest: mt.Destination, File: file, Bytes: n})
	}
	if err := backup.WriteManifest(tmp, m); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			err = fmt.Errorf("%w"+noSpaceHint, errNoSpace)
		}
		return nil, nil, err
	}
	done = true
	// Swap the new copy in. If the last step fails, put the old one back.
	commit = func() error {
		os.RemoveAll(old)
		if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			if perr := os.Rename(old, dir); perr != nil && !errors.Is(perr, fs.ErrNotExist) {
				return fmt.Errorf("%w; putting the previous backup back also failed: %v. It is in %s; rename it to %s", err, perr, old, dir)
			}
			return err
		}
		os.RemoveAll(old)
		return nil
	}
	return m, commit, nil
}

// copyOut writes a tar of path in container id to file and returns its size.
func (g *Gate) copyOut(ctx context.Context, id, path, file string) (int64, error) {
	r, err := g.D.Archive(ctx, id, path)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.ENOSPC) {
		err = fmt.Errorf("%w"+noSpaceHint, errNoSpace)
	}
	return n, err
}
