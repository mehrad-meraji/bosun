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

// takeBackup copies c's mounts into BackupDir/<name> using Docker's archive
// API. c must be stopped, so the files are in a clean state. The new copy
// replaces the old one only when it is complete.
func (g *Gate) takeBackup(ctx context.Context, c *docker.Container) (*backup.Manifest, error) {
	name := strings.TrimPrefix(c.Name, "/")
	if g.BackupDir == "" {
		return nil, fmt.Errorf("%s has %s=true but no backup folder is set; set BOSUN_BACKUP_DIR", name, LabelBackup)
	}
	dir := filepath.Join(g.BackupDir, name)
	// Hidden names (leading dot) so a container literally named "<name>.new"
	// or "<name>.old" never collides with our temp/backup folders — Docker
	// container names must start with a letter or digit, never a dot.
	tmp := filepath.Join(g.BackupDir, "."+name+".new")
	old := filepath.Join(g.BackupDir, "."+name+".old")
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp)
		}
	}()
	// ponytail: the last backup's size is the free-space guess; a disk that
	// fills anyway is caught as ENOSPC during the copy.
	if prev, err := backup.ReadManifest(dir); err == nil {
		if free, err := backup.Free(g.BackupDir); err == nil && free < uint64(prev.Bytes()) {
			return nil, fmt.Errorf("%w: the last backup of %s was %s and only %s is free",
				errNoSpace, name, backup.FormatSize(prev.Bytes()), backup.FormatSize(int64(free)))
		}
	}
	m := &backup.Manifest{Image: c.Image, Time: time.Now().UTC()}
	for i, mt := range backupMounts(c) {
		file := strconv.Itoa(i) + ".tar"
		n, err := g.copyOut(ctx, c.ID, mt.Destination, filepath.Join(tmp, file))
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", mt.Destination, err)
		}
		m.Mounts = append(m.Mounts, backup.Mount{Dest: mt.Destination, File: file, Bytes: n})
	}
	if err := backup.WriteManifest(tmp, m); err != nil {
		return nil, err
	}
	// Swap the new copy in. If the last step fails, put the old one back.
	os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.Rename(old, dir)
		return nil, err
	}
	done = true
	os.RemoveAll(old)
	return m, nil
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
		err = errNoSpace
	}
	return n, err
}
