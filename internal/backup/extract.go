package backup

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrUntouched marks restore errors raised before any data was deleted.
var ErrUntouched = errors.New("nothing was changed")

func untouched(err error) error { return fmt.Errorf("%w; %w", err, ErrUntouched) }

// Restore is the restore helper's job. Each pair is "tarFile=dest", with the
// tar under /backup and dest under /data. It checks every tar first, then
// empties each dest and unpacks its tar into it.
func Restore(pairs []string) error {
	for _, p := range pairs {
		file, dest, _ := strings.Cut(p, "=")
		if !strings.HasPrefix(path.Clean(file), "/backup/") || !strings.HasPrefix(path.Clean(dest), "/data/") {
			return untouched(fmt.Errorf("bad restore argument %q", p))
		}
	}
	return restore(pairs)
}

// restore does the work without the path rules, so tests can use temp dirs.
func restore(pairs []string) error {
	type job struct{ file, dest string }
	var jobs []job
	for _, p := range pairs {
		file, dest, ok := strings.Cut(p, "=")
		if !ok {
			return untouched(fmt.Errorf("bad restore argument %q", p))
		}
		jobs = append(jobs, job{file, dest})
	}
	for _, j := range jobs {
		if err := checkFile(j.file); err != nil {
			return untouched(fmt.Errorf("backup file %s is unreadable: %w", j.file, err))
		}
	}
	for _, j := range jobs {
		if err := Clear(j.dest); err != nil {
			return fmt.Errorf("empty %s: %w", j.dest, err)
		}
		f, err := os.Open(j.file)
		if err != nil {
			return err
		}
		err = Extract(f, j.dest)
		f.Close()
		if err != nil {
			return fmt.Errorf("restore %s: %w", j.dest, err)
		}
	}
	return nil
}

func checkFile(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return Check(f)
}

// Check reads a whole tar, so a cut-off or broken file fails before a restore
// deletes anything.
func Check(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	// Tar files must be a multiple of 512 bytes and end with at least 1024 bytes of zeros (end marker).
	if len(data)%512 != 0 {
		return fmt.Errorf("tar size not a multiple of 512 bytes")
	}
	if len(data) < 1024 {
		return fmt.Errorf("tar too small")
	}

	// Check for end marker (last 1024 bytes should be all zeros).
	for i := len(data) - 1024; i < len(data); i++ {
		if data[i] != 0 {
			return fmt.Errorf("tar missing end marker")
		}
	}

	// Parse the tar to detect any structural errors in entries.
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}

	return nil
}

// Clear deletes everything inside dir but keeps dir, which is a mount point.
func Clear(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Extract unpacks a tar made by Docker's archive API into dest. Docker names
// every entry after the copied folder ("data/a.txt"), so the first path part is
// dropped. Owners, modes and times are kept. Paths that would leave dest,
// directly or through a link, are refused.
// ponytail: devices, fifos and sockets are skipped; app volumes rarely hold them.
func Extract(r io.Reader, dest string) error {
	type dirTime struct {
		path string
		t    time.Time
	}
	var dirs []dirTime
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel, ok := strip(h.Name)
		if !ok {
			return fmt.Errorf("unsafe path in backup: %q", h.Name)
		}
		p := filepath.Join(dest, rel)
		if rel != "" {
			if err := safeParent(dest, rel); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if fi, err := os.Lstat(p); err == nil && !(fi.IsDir() && h.Typeflag == tar.TypeDir) {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{p, h.ModTime})
		case tar.TypeReg:
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			target, ok := strip(h.Linkname)
			if !ok || target == "" {
				return fmt.Errorf("unsafe link in backup: %q", h.Linkname)
			}
			if err := safeParent(dest, target); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(dest, target), p); err != nil {
				return err
			}
			continue // a hard link shares its target's owner and mode; the header's mode is 0
		default:
			continue
		}
		if err := own(p, h); err != nil {
			return err
		}
	}
	// Folder times last, because writing inside a folder changes its time.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].path, dirs[i].t, dirs[i].t)
	}
	return nil
}

// strip drops the first path part and refuses absolute paths and "..".
func strip(name string) (string, bool) {
	name = strings.TrimSuffix(name, "/")
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	parts := strings.Split(name, "/")
	if slices.Contains(parts, "..") {
		return "", false
	}
	return path.Join(parts[1:]...), true
}

// safeParent refuses rel when a folder on the way to it is a link, so a link
// in the backup cannot send a later entry outside dest.
func safeParent(dest, rel string) error {
	dir := dest
	for _, part := range strings.Split(path.Dir(rel), "/") {
		if part == "." || part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unsafe path in backup: %q goes through a link", rel)
		}
	}
	return nil
}

// own sets owner, mode and time. Owner first: chown clears setuid bits.
func own(p string, h *tar.Header) error {
	if err := os.Lchown(p, h.Uid, h.Gid); err != nil {
		return err
	}
	if h.Typeflag == tar.TypeSymlink {
		return nil
	}
	mode := h.FileInfo().Mode()
	if err := os.Chmod(p, mode.Perm()|mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	return os.Chtimes(p, h.ModTime, h.ModTime)
}
