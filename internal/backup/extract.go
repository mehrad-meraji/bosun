package backup

import (
	"archive/tar"
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

// countReader tracks bytes read and keeps a rolling buffer of the last 1024 bytes.
type countReader struct {
	r    io.Reader
	cnt  int64
	last [1024]byte
	pos  int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	// Keep rolling buffer of last 1024 bytes
	for i := 0; i < n; i++ {
		c.last[c.pos] = p[i]
		c.pos = (c.pos + 1) % 1024
	}
	c.cnt += int64(n)
	return n, err
}

func (c *countReader) lastBytes() []byte {
	if c.cnt < 1024 {
		// Haven't read 1024 bytes yet; return what we have
		if c.pos == 0 && c.cnt == 0 {
			return nil
		}
		b := make([]byte, c.cnt)
		if c.cnt <= int64(1024-c.pos) {
			// Data is contiguous in buffer
			copy(b, c.last[1024-int(c.cnt):])
		} else {
			// Data wraps around
			copy(b, c.last[c.pos:])
			copy(b[1024-c.pos:], c.last[:c.pos])
		}
		return b
	}
	// We've read >= 1024 bytes; return the last 1024
	b := make([]byte, 1024)
	copy(b, c.last[c.pos:])
	copy(b[1024-c.pos:], c.last[:c.pos])
	return b
}

// roundUp512 returns n rounded up to the next 512-byte boundary.
func roundUp512(n int64) int64 {
	return (n + 511) / 512 * 512
}

// Check reads a whole tar, so a cut-off or broken file fails before a restore
// deletes anything. It streams the tar in constant memory.
func Check(r io.Reader) error {
	cr := &countReader{r: r}
	tr := tar.NewReader(cr)
	var end int64 // expected byte position after current entry's data

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Verify we read enough bytes (all entries plus 1024-byte end marker)
			if cr.cnt < end+1024 {
				return fmt.Errorf("tar file is cut off")
			}
			// Verify last 1024 bytes are all zeros (end marker)
			last := cr.lastBytes()
			if len(last) < 1024 {
				return fmt.Errorf("tar file is cut off")
			}
			for _, b := range last {
				if b != 0 {
					return fmt.Errorf("tar file is cut off")
				}
			}
			return nil
		}
		if err != nil {
			return err
		}

		// Set expected end position: current count + rounded-up header size
		end = cr.cnt + roundUp512(h.Size)

		// Read (and discard) the entry body
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
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
		rel  string // relative path (for safeParent check)
		path string // absolute path
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
			dirs = append(dirs, dirTime{rel, p, h.ModTime})
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
	// Only set times if the path is safe and is still a directory (not replaced by a symlink).
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if safeParent(dest, d.rel) == nil {
			if fi, err := os.Lstat(d.path); err == nil && fi.IsDir() {
				_ = os.Chtimes(d.path, d.t, d.t)
			}
		}
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
