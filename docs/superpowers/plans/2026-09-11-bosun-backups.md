# Bosun Backups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in volume backups before each update (`bosun.backup=true`), and `bosun rollback <name> --with-data` to put them back.

**Architecture:** Backups use Docker's own copy. The gate streams a tar of each writable mount out of the stopped app (`GET /containers/{id}/archive`) into a gate-only backup folder. No helper container runs for a backup. A restore needs to delete files too, which Docker's copy cannot do, so `--with-data` runs a short, locked-down `restore-helper` container from Bosun's own image: root, no network, read-only root file system, only `CHOWN`, `DAC_OVERRIDE`, `FOWNER`.

**Tech Stack:** Go 1.27 stdlib (`archive/tar`, `syscall.Statfs`), the existing stdlib Docker client, Docker Engine API v1.44.

**Spec:** `docs/superpowers/specs/2026-09-11-bosun-design.md` (sections "Backups", "Restore helper container", "Warnings", "Settings").

## Global Constraints

- Go 1.27. Module `github.com/mehrad-meraji/bosun`. No new outside libraries.
- Docker Engine API pinned to `v1.44`.
- Label: `bosun.backup=true` turns backups on for a container.
- `BOSUN_BACKUP_DIR` default `/var/lib/bosun-backups` (the `bosun-backups` volume). Gate-only: never given to the updater. Must not be under `/run/bosun` or `/etc/bosun`.
- `BOSUN_BACKUP_WARN_SIZE` default `10GB`, 1024-based units.
- One backup per container at `<BOSUN_BACKUP_DIR>/<name>/` holding `manifest.json` and `<i>.tar` files. A new backup replaces the old one only when complete.
- Backups copy writable `volume` and `bind` mounts only; read-only mounts and sources ending in `.sock` are skipped.
- Restore helper: image = the gate's own image by ID; `User` `0:0`; `NetworkMode` `none`; `ReadonlyRootfs` true; `CapDrop` `ALL`; `CapAdd` exactly `CHOWN`, `DAC_OVERRIDE`, `FOWNER`; `no-new-privileges:true`; backup folder mounted read-only; label `bosun.managed-by=bosun-gate`; removed when done. Exit code 2 means nothing was changed.
- Container removal always keeps volumes (`v=0`). Nothing in this plan ever deletes a Docker volume.
- User-facing text uses plain, short English. Every error says what to do next.

## File map

| File | Job |
|---|---|
| `internal/backup/backup.go` | Manifest, sizes, free space. |
| `internal/backup/backup_test.go` | Size parsing and printing, manifest round trip. |
| `internal/backup/extract.go` | Check, Clear, Extract, Restore (the restore helper's job). |
| `internal/backup/extract_test.go` | Safe extraction and restore tests. |
| `internal/docker/docker.go` | Add `Archive`, `Wait`, `Logs`. |
| `internal/gate/backup.go` | Which mounts, take a backup, find the backup for a version. |
| `internal/gate/backup_test.go` | Backup tests with the fake Docker. |
| `internal/gate/restore.go` | Restore helper body and run. |
| `internal/gate/restore_test.go` | Restore helper lockdown test. |
| `internal/gate/gate.go` | Backup inside `swap`, `Rollback(..., withData)`, new fields. |
| `main.go`, `cli.go` | `restore-helper` command, settings, `--with-data`, backup columns. |
| `internal/gate/integration_test.go` | Real-Docker backup and restore flow. |
| `Dockerfile`, `compose.yml`, `README.md` | Backup volume and docs. |

---

### Task 1: Backup basics (manifest, sizes, free space)

**Files:**
- Create: `internal/backup/backup.go`, `internal/backup/backup_test.go`

**Interfaces:**
- Produces:
  - `backup.Manifest{Image string; Time time.Time; Mounts []Mount}` (JSON `image`, `time`, `mounts`), method `(*Manifest).Bytes() int64`
  - `backup.Mount{Dest, File string; Bytes int64}` (JSON `dest`, `file`, `bytes`)
  - `backup.ReadManifest(dir string) (*Manifest, error)` (a missing file gives an error matching `fs.ErrNotExist`)
  - `backup.WriteManifest(dir string, m *Manifest) error`
  - `backup.Free(dir string) (uint64, error)`
  - `backup.ParseSize(s string) (int64, error)`, `backup.FormatSize(n int64) string`

- [ ] **Step 1: Write the failing tests**

`internal/backup/backup_test.go`:

```go
package backup

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"10GB":   10 << 30,
		"500 mb": 500 << 20,
		"1.5GB":  3 << 29,
		"2TB":    2 << 40,
		"4KB":    4 << 10,
		"12B":    12,
		"1024":   1024,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1GB", "GB", "10XB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) must fail", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{
		0:          "0 B",
		900:        "900 B",
		1536:       "1.5 KB",
		2254857830: "2.1 GB",
		3 << 40:    "3.0 TB",
	} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadManifest(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing manifest: got %v, want fs.ErrNotExist", err)
	}
	m := &Manifest{Image: "sha256:abc", Time: time.Now().UTC().Truncate(time.Second),
		Mounts: []Mount{{Dest: "/data", File: "0.tar", Bytes: 100}, {Dest: "/cache", File: "1.tar", Bytes: 23}}}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != m.Image || !got.Time.Equal(m.Time) || len(got.Mounts) != 2 || got.Bytes() != 123 {
		t.Fatalf("got %+v, want %+v with 123 bytes", got, m)
	}
}

func TestFree(t *testing.T) {
	if n, err := Free(t.TempDir()); err != nil || n == 0 {
		t.Fatalf("Free = %d, %v", n, err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/backup/`
Expected: FAIL. `ParseSize` is not defined.

- [ ] **Step 3: Write `internal/backup/backup.go`**

```go
// Package backup holds what backups and restores share: the manifest, sizes,
// free space, and safe tar extraction for the restore helper.
package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Manifest describes one container's backup. It sits next to the tar files.
type Manifest struct {
	Image  string    `json:"image"` // image ID the backup belongs with (the kept old version)
	Time   time.Time `json:"time"`
	Mounts []Mount   `json:"mounts"`
}

type Mount struct {
	Dest  string `json:"dest"` // path inside the app container
	File  string `json:"file"` // tar file name in the backup folder
	Bytes int64  `json:"bytes"`
}

// Bytes is the total size of the backup's tar files.
func (m *Manifest) Bytes() int64 {
	var n int64
	for _, x := range m.Mounts {
		n += x.Bytes
	}
	return n
}

const manifestFile = "manifest.json"

// ReadManifest reads dir/manifest.json. A missing backup matches fs.ErrNotExist.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, manifestFile), err)
	}
	return &m, nil
}

func WriteManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestFile), b, 0o600)
}

// Free returns the bytes a normal user may still write on dir's file system.
func Free(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// units run largest first, so "10GB" matches GB before B.
var units = []struct {
	name string
	n    int64
}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}

// ParseSize reads sizes like "10GB", "500 MB" or "1024". Units are 1024-based.
func ParseSize(s string) (int64, error) {
	bad := fmt.Errorf("bad size %q; use a number and a unit, like 10GB", s)
	t := strings.ToUpper(strings.TrimSpace(s))
	for _, u := range units {
		if num, ok := strings.CutSuffix(t, u.name); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || v < 0 {
				return 0, bad
			}
			return int64(v * float64(u.n)), nil
		}
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v < 0 {
		return 0, bad
	}
	return v, nil
}

// FormatSize prints n in the largest unit that fits, with one decimal: "2.1 GB".
func FormatSize(n int64) string {
	for _, u := range units[:4] {
		if n >= u.n {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(u.n), u.name)
		}
	}
	return fmt.Sprintf("%d B", n)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/backup/ -v && go vet ./...`
Expected: PASS, 4 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/backup/backup.go internal/backup/backup_test.go
git commit -m "Add backup manifest, sizes and free space"
```

---

### Task 2: Safe extraction and the restore job

**Files:**
- Create: `internal/backup/extract.go`, `internal/backup/extract_test.go`

**Interfaces:**
- Consumes: nothing from other packages.
- Produces:
  - `backup.Check(r io.Reader) error`: reads a tar to the end
  - `backup.Clear(dir string) error`: empties dir, keeps dir
  - `backup.Extract(r io.Reader, dest string) error`
  - `backup.Restore(pairs []string) error`: the restore helper's job; each pair is `"/backup/<name>/<i>.tar=/data/<i>"`
  - `backup.ErrUntouched`: wrapped by Restore errors raised before anything was deleted

- [ ] **Step 1: Write the failing tests**

`internal/backup/extract_test.go`:

```go
package backup

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// mkTar builds a tar like Docker's archive API makes, owned by this user.
func mkTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link,
			Size: int64(len(e.body)), Uid: os.Getuid(), Gid: os.Getgid(), ModTime: time.Unix(1700000000, 0)}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClearKeepsDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600)
	if err := Clear(dir); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestExtract(t *testing.T) {
	dest := t.TempDir()
	tb := mkTar(t,
		entry{name: "data/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "data/a.txt", typ: tar.TypeReg, mode: 0o640, body: "hi"},
		entry{name: "data/sub/", typ: tar.TypeDir, mode: 0o700},
		entry{name: "data/sub/b.txt", typ: tar.TypeReg, mode: 0o600, body: "deep"},
		entry{name: "data/link", typ: tar.TypeSymlink, link: "a.txt", mode: 0o777},
		entry{name: "data/hard", typ: tar.TypeLink, link: "data/a.txt"},
	)
	if err := Extract(bytes.NewReader(tb), dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "sub", "b.txt")); string(b) != "deep" {
		t.Errorf("sub/b.txt = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("a.txt mode = %v, %v; want 0640", fi.Mode().Perm(), err)
	}
	if l, err := os.Readlink(filepath.Join(dest, "link")); err != nil || l != "a.txt" {
		t.Errorf("link = %q, %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "hard")); string(b) != "hi" {
		t.Errorf("hard link = %q", b)
	}
}

func TestExtractRefusesClimbingOut(t *testing.T) {
	outside := t.TempDir()
	for _, tb := range [][]byte{
		mkTar(t, entry{name: "data/../../evil", typ: tar.TypeReg, mode: 0o600, body: "x"}),
		mkTar(t, entry{name: "/etc/evil", typ: tar.TypeReg, mode: 0o600, body: "x"}),
		// A link to a folder outside, then a file written through it.
		mkTar(t,
			entry{name: "data/l", typ: tar.TypeSymlink, link: outside},
			entry{name: "data/l/evil", typ: tar.TypeReg, mode: 0o600, body: "x"}),
	} {
		if err := Extract(bytes.NewReader(tb), t.TempDir()); err == nil {
			t.Error("Extract must refuse a path that leaves dest")
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a file was written outside dest")
	}
}

func TestCheckFindsBrokenTar(t *testing.T) {
	tb := mkTar(t, entry{name: "data/a.txt", typ: tar.TypeReg, mode: 0o600, body: "hello world"})
	if err := Check(bytes.NewReader(tb)); err != nil {
		t.Fatal(err)
	}
	if err := Check(bytes.NewReader(tb[:600])); err == nil {
		t.Fatal("Check must fail on a cut-off tar")
	}
}

func TestRestore(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "0.tar")
	os.WriteFile(good, mkTar(t, entry{name: "data/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "data/f", typ: tar.TypeReg, mode: 0o600, body: "old"}), 0o600)
	dest := filepath.Join(tmp, "dest")
	os.MkdirAll(dest, 0o755)
	os.WriteFile(filepath.Join(dest, "f"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(dest, "made-later"), []byte("x"), 0o600)

	// Restore only accepts /backup and /data paths; test the rules first.
	if err := Restore([]string{good + "=" + dest}); !errors.Is(err, ErrUntouched) {
		t.Fatalf("paths outside /backup and /data must be refused untouched, got %v", err)
	}
	if err := restore([]string{good + "=" + dest}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "f")); string(b) != "old" {
		t.Errorf("f = %q, want old", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "made-later")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a file made after the backup survived the restore")
	}

	// A broken tar is found before anything is deleted.
	bad := filepath.Join(tmp, "1.tar")
	os.WriteFile(bad, []byte("not a tar at all, just some bytes that are long enough to fail"), 0o600)
	os.WriteFile(filepath.Join(dest, "keep"), []byte("x"), 0o600)
	if err := restore([]string{good + "=" + dest, bad + "=" + dest}); !errors.Is(err, ErrUntouched) {
		t.Fatalf("want ErrUntouched, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "keep")); err != nil {
		t.Error("data was deleted although a tar was broken")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/backup/`
Expected: FAIL. `Extract` is not defined.

- [ ] **Step 3: Write `internal/backup/extract.go`**

```go
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

// Check reads a whole tar, so a cut-off or broken file fails before a restore
// deletes anything.
func Check(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
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
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/backup/ -v && go vet ./...`
Expected: PASS, 9 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/backup/extract.go internal/backup/extract_test.go
git commit -m "Add safe tar extraction and the restore job"
```

---

### Task 3: Docker client: archive, wait, logs

**Files:**
- Modify: `internal/docker/docker.go` (add three methods after `RemoveImage`)
- Modify: `internal/docker/docker_test.go` (add tests)

**Interfaces:**
- Produces:
  - `(*docker.Client).Archive(ctx, id, path string) (io.ReadCloser, error)`
  - `(*docker.Client).Wait(ctx, id string) (int, error)`
  - `(*docker.Client).Logs(ctx, id string) (string, error)`

- [ ] **Step 1: Write the failing tests**

Add to `internal/docker/docker_test.go`:

```go
func TestArchive(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/abc/archive" || r.URL.Query().Get("path") != "/var/lib/data" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, "TARBYTES")
	})
	rc, err := c.Archive(context.Background(), "abc", "/var/lib/data")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "TARBYTES" {
		t.Fatalf("body = %q", b)
	}
}

func TestWait(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.44/containers/abc/wait" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"StatusCode":2}`)
	})
	if code, err := c.Wait(context.Background(), "abc"); err != nil || code != 2 {
		t.Fatalf("Wait = %d, %v; want 2", code, err)
	}
}

func TestWaitError(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"StatusCode":0,"Error":{"Message":"container gone"}}`)
	})
	if _, err := c.Wait(context.Background(), "abc"); err == nil || !strings.Contains(err.Error(), "container gone") {
		t.Fatalf("want the wait error, got %v", err)
	}
}

func TestLogs(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1.44/containers/abc/logs" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		io.WriteString(w, "bosun: backup file is unreadable\n")
	})
	if out, err := c.Logs(context.Background(), "abc"); err != nil || out != "bosun: backup file is unreadable" {
		t.Fatalf("Logs = %q, %v", out, err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/docker/`
Expected: FAIL. `c.Archive` is not defined.

- [ ] **Step 3: Add the methods to `internal/docker/docker.go`**

Add after `RemoveImage`:

```go
// Archive streams a tar of path from a container, running or stopped. Docker
// names each entry after the last part of path. The caller closes it.
func (c *Client) Archive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {path}}, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Wait blocks until the container stops and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := c.call(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil, &out); err != nil {
		return 0, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// Logs returns the last lines a container printed, up to 4 KB. The container
// must use Tty: true, so the output is plain text without Docker's framing.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"50"}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.TrimSpace(string(b)), err
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/docker/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/docker/docker.go internal/docker/docker_test.go
git commit -m "Add Docker archive, wait and logs calls"
```

---

### Task 4: Back up during an update

**Files:**
- Create: `internal/gate/backup.go`, `internal/gate/backup_test.go`
- Modify: `internal/gate/gate.go` (constants, `Gate`, `Watched`, `Result`, `List`, `Update`, `swap`, `Rollback`'s swap call)
- Modify: `internal/state/state.go` (`Entry.WarnedBig`)
- Modify: `internal/gate/boot_test.go` (one more assertion)

**Interfaces:**
- Consumes: `backup.Manifest`, `backup.Mount`, `backup.ReadManifest`, `backup.WriteManifest`, `backup.Free`, `backup.FormatSize` (Task 1); `docker.Client.Archive` (Task 3).
- Produces:
  - `gate.LabelBackup = "bosun.backup"`
  - `Gate` fields `BackupDir string`, `WarnSize int64`, `BackupSrc string`, `HelperImage string` (the last two are used in Task 5)
  - `Watched.Backup bool` (JSON `backup`)
  - `Result.Backup time.Duration` (JSON `backup,omitempty`), `Result.BackupBytes int64` (JSON `backup_bytes,omitempty`)
  - `state.Entry.WarnedBig bool` (JSON `warned_big,omitempty`)
  - `backupMounts(c *docker.Container) []docker.Mount`, `(*Gate).takeBackup(ctx, c *docker.Container) (*backup.Manifest, error)`
  - `swap(ctx, f, st, old, ref, digest string, withBackup bool)` — new last parameter

- [ ] **Step 1: Write the failing tests**

`internal/gate/backup_test.go`:

```go
package gate

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

// backupCtr is oldCtr with backups on and one writable volume at /data.
var backupCtr = strings.Replace(
	strings.Replace(oldCtr, `"Mounts":[]`, `"Mounts":[{"Type":"volume","Name":"appdata","Destination":"/data","RW":true}]`, 1),
	`"bosun.enable":"true"`, `"bosun.enable":"true","bosun.backup":"true"`, 1)

func dataTar(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "data/f.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2})
	tw.Write([]byte("hi"))
	tw.Close()
	return buf.String()
}

func backupFake(t *testing.T, newRunning bool) (*dockerFake, *Gate) {
	f := swapFake(newRunning)
	f.containers["app"] = backupCtr
	f.containers["old-id"] = backupCtr
	f.containers["old-id/archive"] = dataTar(t)
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	return f, g
}

func TestBackupMounts(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{
		{Type: "volume", Name: "v", Destination: "/v", RW: true},
		{Type: "bind", Source: "/srv/x", Destination: "/x", RW: true},
		{Type: "volume", Name: "ro", Destination: "/ro", RW: false},
		{Type: "tmpfs", Destination: "/tmp", RW: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
	}}
	got := backupMounts(c)
	if len(got) != 2 || got[0].Destination != "/v" || got[1].Destination != "/x" {
		t.Fatalf("backupMounts = %+v, want /v and /x only", got)
	}
}

func TestTakeBackupReplacesTheOldOne(t *testing.T) {
	f, g := backupFake(t, true)
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "junk"), []byte("old"), 0o600)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:older"})

	c, err := g.D.Inspect(context.Background(), "old-id")
	if err != nil {
		t.Fatal(err)
	}
	m, err := g.takeBackup(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if m.Image != "sha256:old" || len(m.Mounts) != 1 || m.Mounts[0].Dest != "/data" || m.Mounts[0].Bytes != int64(len(f.containers["old-id/archive"])) {
		t.Fatalf("manifest = %+v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "0.tar")); string(b) != f.containers["old-id/archive"] {
		t.Error("0.tar does not hold the archive")
	}
	if _, err := os.Stat(filepath.Join(dir, "junk")); err == nil {
		t.Error("the old backup was not replaced")
	}
	for _, left := range []string{dir + ".new", dir + ".old"} {
		if _, err := os.Stat(left); err == nil {
			t.Errorf("%s left behind", left)
		}
	}
}

func TestTakeBackupFailureKeepsTheOldOne(t *testing.T) {
	f, g := backupFake(t, true)
	f.fail["GET /containers/old-id/archive"] = true
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:older"})
	c, _ := g.D.Inspect(context.Background(), "old-id")
	if _, err := g.takeBackup(context.Background(), c); err == nil {
		t.Fatal("want an error")
	}
	if m, err := backup.ReadManifest(dir); err != nil || m.Image != "sha256:older" {
		t.Errorf("old backup changed: %+v %v", m, err)
	}
	if _, err := os.Stat(dir + ".new"); err == nil {
		t.Error("partial copy left behind")
	}
}

func TestUpdateBacksUpWhileStopped(t *testing.T) {
	f, g := backupFake(t, true)
	g.WarnSize = 1 // any backup is "big", to test the one-time warning
	res, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err != nil || res.Status != StatusDone {
		t.Fatalf("Update = %+v %v", res, err)
	}
	if !f.calledAfter("POST /containers/old-id/stop", "GET /containers/old-id/archive?path=/data") {
		t.Errorf("backup must happen after the stop; calls: %v", f.calls)
	}
	if !f.calledAfter("GET /containers/old-id/archive?path=/data", "POST /containers/create?name=app") {
		t.Errorf("backup must happen before the new container; calls: %v", f.calls)
	}
	if res.BackupBytes == 0 || !strings.Contains(res.Message, "backup") || !strings.Contains(res.Message, "long downtime") {
		t.Errorf("result = %+v, want backup size, time and a warning", res)
	}
	if st, _ := state.Read(g.Dir); !st.Entry("app").WarnedBig {
		t.Error("warning must be remembered")
	}
	// Second time: no repeat of the warning.
	f.containers["app"] = backupCtr
	res, _ = g.Update(context.Background(), "app", "sha256:d2", "")
	if strings.Contains(res.Message, "long downtime") {
		t.Errorf("warning repeated: %s", res.Message)
	}
}

func TestUpdateBackupFailureStopsTheUpdate(t *testing.T) {
	f, g := backupFake(t, true)
	f.fail["GET /containers/old-id/archive"] = true
	_, err := g.Update(context.Background(), "app", "sha256:d2", "")
	if err == nil || !strings.Contains(err.Error(), "backup failed") || !strings.Contains(err.Error(), "running again") {
		t.Fatalf("want a backup failure that says the old version runs, got %v", err)
	}
	if f.called("POST /containers/create?name=app") {
		t.Error("updated without a backup")
	}
	if !f.calledAfter("GET /containers/old-id/archive?path=/data", "POST /containers/old-id/start") {
		t.Errorf("old container not started again; calls: %v", f.calls)
	}
	if st, _ := state.Read(g.Dir); len(st.Pending) != 0 {
		t.Errorf("pending record left: %+v", st.Pending)
	}
}

func TestNoBackupWithoutLabel(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	if _, err := g.Update(context.Background(), "app", "sha256:d2", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/archive") {
			t.Fatalf("backed up without bosun.backup=true: %v", f.calls)
		}
	}
}
```

In `internal/gate/boot_test.go`, in `TestUpdaterBodyIsLockedDown`, add a gate mount `{Type: "volume", Name: "bosun-backups", Destination: "/var/lib/bosun-backups"}` to `self.Mounts`, and add `"bosun-backups"` and `"/var/lib/bosun-backups"` to the list of strings the body must NOT contain.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/gate/`
Expected: FAIL. `backupMounts` is not defined.

- [ ] **Step 3: Write `internal/gate/backup.go`**

```go
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
	tmp, old := dir+".new", dir+".old"
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
```

- [ ] **Step 4: Change `internal/state/state.go`**

Add a field to `Entry`, after `Skip`:

```go
	WarnedBig bool          `json:"warned_big,omitempty"` // the big-backup warning went out once
```

- [ ] **Step 5: Change `internal/gate/gate.go`**

1. Add `"github.com/mehrad-meraji/bosun/internal/backup"` to the imports.
2. Add to the label constants: `LabelBackup  = "bosun.backup"`.
3. Add fields to `Gate`, after `RunDir`:

```go
	BackupDir   string // gate-only backup folder, /var/lib/bosun-backups; never given to the updater
	WarnSize    int64  // a backup bigger than this gets a one-time warning; 0 means never
	BackupSrc   string // BackupDir as Docker sees it (volume name or host path); found from the gate's mounts if empty
	HelperImage string // image for the restore helper; the gate's own image if empty
```

4. Add a field to `Watched`, after `Skip`: `Backup  bool     \`json:"backup"\``. In `List`, set it: `Backup: c.Config.Labels[LabelBackup] == "true"` in the `Watched{...}` literal.
5. Add fields to `Result`, after `Downtime`:

```go
	Backup      time.Duration `json:"backup,omitempty"`       // time the backup took, inside Downtime
	BackupBytes int64         `json:"backup_bytes,omitempty"` // size of the backup
```

6. In `Update`, replace `res, err := g.swap(ctx, f, st, c, ref, digest)` with:

```go
	res, err := g.swap(ctx, f, st, c, ref, digest, c.Config.Labels[LabelBackup] == "true")
```

and, just before the final `return res, f.Save(st)` of `Update`, add:

```go
	if g.WarnSize > 0 && res.BackupBytes > g.WarnSize && !e.WarnedBig {
		res.Message += fmt.Sprintf(". Warning: the backup of %s is %s, so its updates have long downtime", name, backup.FormatSize(res.BackupBytes))
		e.WarnedBig = true
	}
```

7. In `Rollback`, change the swap call to `g.swap(ctx, f, st, c, ref, "", false)`.
8. Change `swap`'s signature and doc comment:

```go
// swap replaces old with a new container running ref, then waits for it to
// be healthy. If anything fails after the old one stops, it puts the old one
// back and returns StatusReverted. digest is the version an update goes to,
// or "" for a rollback; crash recovery reads it. withBackup copies the old
// container's mounts while it is stopped, before anything else changes.
func (g *Gate) swap(ctx context.Context, f *state.File, st *state.State, old *docker.Container, ref, digest string, withBackup bool) (Result, error) {
```

9. In `swap`, right after the `Stop` error check and before the `Rename`, add:

```go
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
```

10. In `swap`, change the success return at the end to:

```go
	msg := fmt.Sprintf("%s: now running %s (down %s)", name, ref, down.Round(100*time.Millisecond))
	if withBackup {
		msg = fmt.Sprintf("%s: now running %s (down %s, backup %s, %s)", name, ref,
			down.Round(100*time.Millisecond), took.Round(100*time.Millisecond), backup.FormatSize(size))
	}
	return Result{Status: StatusDone, Downtime: down, Backup: took, BackupBytes: size, Message: msg}, nil
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `go test ./... && go vet ./...`
Expected: PASS, including the six new gate tests and every earlier test.

- [ ] **Step 7: Commit**

```bash
git add internal/gate/backup.go internal/gate/backup_test.go internal/gate/gate.go internal/gate/boot_test.go internal/state/state.go
git commit -m "Back up volumes before an update when bosun.backup=true"
```

---

### Task 5: Restore helper and `Rollback --with-data`

**Files:**
- Create: `internal/gate/restore.go`, `internal/gate/restore_test.go`
- Modify: `internal/gate/gate.go` (`Rollback` gains `withData bool`)
- Modify: `main.go` (`restore-helper` command; exit code 2)
- Modify: `cli.go` (call `Rollback(ctx, name, false)` so it compiles; Task 6 wires the flag)
- Modify: `internal/gate/gate_test.go` (`g.Rollback(context.Background(), "app")` → `g.Rollback(context.Background(), "app", false)`)
- Modify: `internal/gate/integration_test.go` (`g.Rollback(ctx, name)` → `g.Rollback(ctx, name, false)`)

**Interfaces:**
- Consumes: `backup.Manifest`, `backup.ReadManifest`, `backup.Restore`, `backup.ErrUntouched` (Tasks 1-2); `docker.Client.Wait`, `Logs` (Task 3); `Gate.BackupDir`, `BackupSrc`, `HelperImage` (Task 4).
- Produces:
  - `(*Gate).Rollback(ctx, name string, withData bool) (Result, error)`
  - `(*Gate).DataBackup(ctx, name, prev string) (*backup.Manifest, error)`: the backup that belongs with the kept version, or an error saying why not
  - `restoreBody(image, backupSrc, name string, m *backup.Manifest, c *docker.Container) (map[string]any, error)`
  - `bosun restore-helper <tar=dest>...`: exit 0 ok, 2 nothing changed, 1 data incomplete

- [ ] **Step 1: Write the failing tests**

`internal/gate/restore_test.go`:

```go
package gate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/backup"
	"github.com/mehrad-meraji/bosun/internal/docker"
	"github.com/mehrad-meraji/bosun/internal/state"
)

func TestRestoreBodyIsLockedDown(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{
		{Type: "volume", Name: "appdata", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/app/conf", Destination: "/conf", RW: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
	}}
	m := &backup.Manifest{Mounts: []backup.Mount{{Dest: "/data", File: "0.tar"}, {Dest: "/conf", File: "1.tar"}}}
	body, err := restoreBody("sha256:bosun", "bosun-backups", "app", m, c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	s := string(b)
	for _, want := range []string{
		`"Image":"sha256:bosun"`,
		`"User":"0:0"`,
		`"NetworkMode":"none"`,
		`"ReadonlyRootfs":true`,
		`"CapDrop":["ALL"]`,
		`"CapAdd":["CHOWN","DAC_OVERRIDE","FOWNER"]`,
		`"no-new-privileges:true"`,
		`"bosun-backups:/backup:ro"`,
		`"appdata:/data/0"`,
		`"/srv/app/conf:/data/1"`,
		`"/backup/app/0.tar=/data/0"`,
		`"/backup/app/1.tar=/data/1"`,
		`"bosun.managed-by":"bosun-gate"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("restore body lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "docker.sock") {
		t.Errorf("restore helper must never get docker.sock: %s", s)
	}
}

func TestRestoreBodyRefusesBadInput(t *testing.T) {
	c := &docker.Container{Mounts: []docker.Mount{{Type: "volume", Name: "appdata", Destination: "/data", RW: true}}}
	for _, m := range []*backup.Manifest{
		{Mounts: []backup.Mount{{Dest: "/gone", File: "0.tar"}}},       // mount no longer there
		{Mounts: []backup.Mount{{Dest: "/data", File: "../x.tar"}}},    // odd file name
		{Mounts: []backup.Mount{{Dest: "/data", File: "0.tar:/etc"}}}, // bind injection
	} {
		if _, err := restoreBody("img", "src", "app", m, c); err == nil {
			t.Errorf("restoreBody(%+v) must fail", m.Mounts)
		}
	}
}

func TestDataBackupMustMatchTheKeptVersion(t *testing.T) {
	f := swapFake(true)
	f.images["bosun/prev/app:abc"] = `{"Id":"sha256:prev"}`
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	if _, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err == nil || !strings.Contains(err.Error(), "bosun.backup=true") {
		t.Fatalf("no backup: want a hint about the label, got %v", err)
	}
	dir := filepath.Join(g.BackupDir, "app")
	os.MkdirAll(dir, 0o700)
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:other"})
	if _, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("wrong version: got %v", err)
	}
	backup.WriteManifest(dir, &backup.Manifest{Image: "sha256:prev"})
	if m, err := g.DataBackup(context.Background(), "app", "bosun/prev/app:abc"); err != nil || m.Image != "sha256:prev" {
		t.Fatalf("matching backup: %+v %v", m, err)
	}
}

func TestRollbackWithDataRefusesBeforeChangingAnything(t *testing.T) {
	f := swapFake(true)
	f.images["bosun/prev/app:abc"] = `{"Id":"sha256:prev"}`
	g := f.gate(t)
	g.BackupDir = t.TempDir()
	setEntry(t, g, "app", state.Entry{Prev: "bosun/prev/app:abc"})
	if _, err := g.Rollback(context.Background(), "app", true); err == nil {
		t.Fatal("want an error: there is no data backup")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST") {
			t.Fatalf("changed something before refusing: %v", f.calls)
		}
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/gate/`
Expected: FAIL. `restoreBody` is not defined.

- [ ] **Step 3: Write `internal/gate/restore.go`**

```go
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
		if src == "" && m.Destination == g.BackupDir {
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
	if code == 2 {
		return fmt.Errorf("restore helper refused: %s (%w)", out, errUntouched)
	}
	return fmt.Errorf("restore helper failed (exit %d): %s", code, out)
}
```

- [ ] **Step 4: Change `Rollback` in `internal/gate/gate.go`**

1. Change the signature and doc comment:

```go
// Rollback puts back the version kept by the last update. withData also puts
// the volumes back from the backup that belongs with that version.
func (g *Gate) Rollback(ctx context.Context, name string, withData bool) (Result, error) {
```

2. After the `cur, err := g.D.InspectImage(ctx, c.Image)` error check, and before `repo, tag := docker.SplitRef(ref)`, add:

```go
	var m *backup.Manifest
	if withData {
		if m, err = g.DataBackup(ctx, name, e.Prev); err != nil {
			return Result{}, err
		}
	}
```

3. Right after the `defer func() { if !ok { g.retag(...) } }()` block and before `res, err := g.swap(...)`, add:

```go
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
```

4. Replace the `if res.Status == StatusReverted { ... }` block inside `Rollback` with:

```go
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
```

5. In the success message at the end of `Rollback`, when `withData`, add the data note. Replace the `res.Message = fmt.Sprintf("%s: rolled back ...` line with:

```go
	res.Message = fmt.Sprintf("%s: rolled back (down %s). The newer version is on the skip list; `bosun skip clear %s` allows it again",
		name, res.Downtime.Round(100*time.Millisecond), name)
	if withData {
		res.Message += fmt.Sprintf(". Data is from the backup of %s", m.Time.Format("2006-01-02 15:04"))
	}
```

- [ ] **Step 5: Add the `restore-helper` command to `main.go`**

1. Add imports `"errors"` and `"github.com/mehrad-meraji/bosun/internal/backup"`.
2. In `run`, add a case before `"help"`:

```go
	case "restore-helper":
		return backup.Restore(args)
```

3. In `main`, replace the error block with:

```go
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
		if errors.Is(err, backup.ErrUntouched) {
			os.Exit(2) // the restore helper changed nothing
		}
		os.Exit(1)
	}
```

4. In `cli.go`, change `newGate().Rollback(ctx, name)` to `newGate().Rollback(ctx, name, false)`.
5. In `internal/gate/gate_test.go` and `internal/gate/integration_test.go`, add `false` as the last argument of every `Rollback` call.

- [ ] **Step 6: Run the tests to see them pass**

Run: `go build ./... && go test ./... && go vet ./... && go vet -tags integration ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/gate/restore.go internal/gate/restore_test.go internal/gate/gate.go internal/gate/gate_test.go internal/gate/integration_test.go main.go cli.go
git commit -m "Add the restore helper and rollback --with-data"
```

---

### Task 6: Settings and CLI

**Files:**
- Modify: `main.go` (settings, checks, `newGate`)
- Modify: `cli.go` (`status`, `rollback ls`, `rollback show`, `--with-data`, help)
- Modify: `cli_test.go` (new tests)

**Interfaces:**
- Consumes: `gate.Watched.Backup`, `gate.Gate.DataBackup`, `gate.Gate.Rollback(ctx, name, withData)` (Tasks 4-5); `backup.ParseSize`, `backup.FormatSize`, `backup.ReadManifest` (Task 1).
- Produces: `inside(child, parent string) bool`, `checkSettings() error`, `backupCol(w gate.Watched) string` in package main.

- [ ] **Step 1: Write the failing tests**

Add to `cli_test.go`:

```go
func TestInside(t *testing.T) {
	for _, tc := range []struct {
		child, parent string
		want          bool
	}{
		{"/run/bosun", "/run/bosun", true},
		{"/run/bosun/state", "/run/bosun", true},
		{"/run/bosun-backups", "/run/bosun", false},
		{"/var/lib/bosun-backups", "/etc/bosun", false},
		{"/etc/bosun/../bosun/x", "/etc/bosun", true},
	} {
		if got := inside(tc.child, tc.parent); got != tc.want {
			t.Errorf("inside(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}

func TestBackupCol(t *testing.T) {
	dir := t.TempDir()
	backupDir = dir
	if got := backupCol(gate.Watched{Name: "web"}); got != "off" {
		t.Errorf("no label: %q", got)
	}
	if got := backupCol(gate.Watched{Name: "web", Backup: true}); got != "on, none yet" {
		t.Errorf("no backup yet: %q", got)
	}
	os.MkdirAll(filepath.Join(dir, "web"), 0o700)
	backup.WriteManifest(filepath.Join(dir, "web"), &backup.Manifest{Mounts: []backup.Mount{{Bytes: 3 << 30}}})
	if got := backupCol(gate.Watched{Name: "web", Backup: true}); got != "on, 3.0 GB" {
		t.Errorf("with backup: %q", got)
	}
}
```

Add imports to `cli_test.go`: `"os"`, `"path/filepath"`, `"github.com/mehrad-meraji/bosun/internal/backup"`, `"github.com/mehrad-meraji/bosun/internal/gate"`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test .`
Expected: FAIL. `inside` is not defined.

- [ ] **Step 3: Change `main.go`**

1. Add to the `var (...)` block:

```go
	backupDir  = env("BOSUN_BACKUP_DIR", "/var/lib/bosun-backups")
	warnSize   = env("BOSUN_BACKUP_WARN_SIZE", "10GB")
```

2. Replace `newGate` with:

```go
func newGate() *gate.Gate {
	self, _ := os.Hostname() // Docker sets it to the short container ID
	warn, _ := backup.ParseSize(warnSize) // checked at gate start by checkSettings
	return &gate.Gate{D: docker.New(dockerSock), Dir: stateDir, RunDir: runDir, BackupDir: backupDir,
		WarnSize: warn, SelfID: self}
}

// checkSettings refuses settings that would hand gate-only folders to the
// updater, which gets RunDir and /etc/bosun.
func checkSettings() error {
	if _, err := backup.ParseSize(warnSize); err != nil {
		return fmt.Errorf("BOSUN_BACKUP_WARN_SIZE: %w", err)
	}
	for _, d := range []struct{ name, path string }{{"BOSUN_STATE_DIR", stateDir}, {"BOSUN_BACKUP_DIR", backupDir}} {
		for _, shared := range []string{runDir, "/etc/bosun"} {
			if inside(d.path, shared) {
				return fmt.Errorf("%s (%s) is inside %s, which the updater can read; pick a folder outside it", d.name, d.path, shared)
			}
		}
	}
	return nil
}

func inside(child, parent string) bool {
	child, parent = filepath.Clean(child), filepath.Clean(parent)
	return child == parent || strings.HasPrefix(child, parent+"/")
}
```

3. At the top of `runGate`, add:

```go
	if err := checkSettings(); err != nil {
		return err
	}
```

- [ ] **Step 4: Change `cli.go`**

1. Add imports `"github.com/mehrad-meraji/bosun/internal/backup"`.
2. Replace the `"rollback"` help entry with:

```go
	"rollback": "bosun rollback ls\nbosun rollback show <name> [--with-data]\nbosun rollback <name> [--with-data] [--dry-run] [--yes]\n" +
		"  Go back to the version kept by the last update. --with-data also puts the volumes back from\n" +
		"  the backup made before that update (containers with bosun.backup=true). It asks before it acts.\n" +
		"  Example: docker exec -it bosun-gate bosun rollback nginx --with-data",
```

3. In `cmdStatus`, change the header and row to add a BACKUP column:

```go
	fmt.Fprintln(tw, "NAME\tMODE\tIMAGE\tLAST UPDATE\tDOWNTIME\tBACKUP\tROLLBACK KEPT")
	for _, w := range ws {
		e := st.Entry(w.Name)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.Name, w.Mode, w.Ref, ago(e.UpdatedAt), dur(e.Downtime), backupCol(w), yesNo(e.Prev != ""))
	}
```

and add:

```go
// backupCol says whether backups are on and how big the last one is.
func backupCol(w gate.Watched) string {
	if !w.Backup {
		return "off"
	}
	m, err := backup.ReadManifest(filepath.Join(backupDir, w.Name))
	if err != nil {
		return "on, none yet"
	}
	return "on, " + backup.FormatSize(m.Bytes())
}
```

4. In `cmdRollback`: allow the flag with `checkFlags(flags, "dry-run", "yes", "with-data")`; pass it through: `rollbackShow(ctx, st, pos[1], flags["with-data"])` in the `show` case, `rollbackShow(ctx, st, name, flags["with-data"])` before the prompt, and `newGate().Rollback(ctx, name, flags["with-data"])`.
5. In `rollbackLs`, add a DATA BACKUP column:

```go
	fmt.Fprintln(tw, "NAME\tNOW\tBACK TO\tUPDATED\tDATA BACKUP")
```

and in the loop, before the `Fprintf`:

```go
		data := "no"
		if m, err := newGate().DataBackup(ctx, name, e.Prev); err == nil {
			data = "yes, " + backup.FormatSize(m.Bytes())
		}
```

with the row `fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, now, e.Prev, ago(e.UpdatedAt), data)`.

6. Replace `rollbackShow` with:

```go
func rollbackShow(ctx context.Context, st *state.State, name string, withData bool) error {
	e := st.Containers[name]
	if e == nil || e.Prev == "" {
		return fmt.Errorf("no old version kept for %s. Run `bosun rollback ls` to see what you can roll back", name)
	}
	c, err := docker.New(dockerSock).Inspect(ctx, name)
	if err != nil {
		return err
	}
	m, dataErr := newGate().DataBackup(ctx, name, e.Prev)
	fmt.Printf("%s now runs %s (image %s).\n", name, c.Config.Image, short(c.Image))
	fmt.Printf("A rollback puts back %s, from the update %s.\n", e.Prev, ago(e.UpdatedAt))
	switch {
	case withData && dataErr != nil:
		return dataErr
	case withData:
		fmt.Printf("With --with-data, the volumes go back to the backup from %s (%s). Data written since then is lost.\n",
			ago(m.Time), backup.FormatSize(m.Bytes()))
		fmt.Printf("A short helper container does this. It runs as root, with no network, and only sees %s's folders.\n", name)
	case dataErr == nil:
		fmt.Printf("Volumes are not changed. A data backup from %s is kept; add --with-data to put it back too.\n", ago(m.Time))
	default:
		fmt.Println("Volumes are not changed. Data written by the newer version stays.")
	}
	fmt.Println("The newer version goes on the skip list.")
	flag := ""
	if withData {
		flag = " --with-data"
	}
	fmt.Printf("To do it: docker exec -it bosun-gate bosun rollback %s%s\n", name, flag)
	return nil
}
```

- [ ] **Step 5: Run the tests and build**

Run: `go test ./... && go vet ./... && go build -o /tmp/bosun-t6 . && /tmp/bosun-t6 help rollback`
Expected: PASS, then the new rollback help.

- [ ] **Step 6: Commit**

```bash
git add main.go cli.go cli_test.go
git commit -m "Add backup settings, status column and rollback --with-data"
```

---

### Task 7: Backup and restore on real Docker

**Files:**
- Modify: `internal/gate/integration_test.go`

**Interfaces:**
- Consumes: `gate.Gate` fields `BackupDir`, `BackupSrc`, `HelperImage`; `Update`; `Rollback(ctx, name, true)`; `backup.ReadManifest`.

The restore helper runs Bosun's own image, so this test builds it once from the repo root.

- [ ] **Step 1: Write the test**

Add to `internal/gate/integration_test.go` (imports: add `"github.com/mehrad-meraji/bosun/internal/backup"` and `"sync"`):

```go
var helperOnce sync.Once

// helperImage builds Bosun's image once, for the restore helper.
func helperImage(t *testing.T) string {
	t.Helper()
	const tag = "bosun-it-helper:latest"
	helperOnce.Do(func() { sh(t, "docker", "build", "-q", "-t", tag, "../..") })
	return tag
}

func TestBackupAndRestoreData(t *testing.T) {
	g, name := setup(t)
	ctx := context.Background()
	vol := name + "-named"
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "-v", name).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
	})
	// A host folder the gate writes and Docker can bind into the helper.
	dir, err := os.MkdirTemp("", "bosun-it-backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g.BackupDir, g.BackupSrc, g.HelperImage = dir, dir, helperImage(t)

	push(t, v1)
	runApp(t, name, "--label", "bosun.backup=true", "-v", vol+":/named", "--mount", "type=volume,dst=/anon")
	sh(t, "docker", "exec", name, "sh", "-c", "echo v1 > /named/f && echo v1 > /anon/f")

	d2 := push(t, v2)
	res, err := g.Update(ctx, name, d2, "")
	if err != nil || res.Status != gate.StatusDone || res.BackupBytes == 0 {
		t.Fatalf("update with backup: %+v %v", res, err)
	}
	m, err := backup.ReadManifest(filepath.Join(dir, name))
	if err != nil || len(m.Mounts) != 2 {
		t.Fatalf("manifest = %+v %v, want 2 mounts", m, err)
	}

	sh(t, "docker", "exec", name, "sh", "-c", "echo v2 > /named/f && echo later > /named/new.txt && echo v2 > /anon/f")
	res, err = g.Rollback(ctx, name, true)
	if err != nil || res.Status != gate.StatusDone {
		t.Fatalf("rollback --with-data: %+v %v", res, err)
	}
	if v := version(t, name); v != "v1" {
		t.Fatalf("running %s, want v1", v)
	}
	for file, want := range map[string]string{"/named/f": "v1", "/anon/f": "v1"} {
		if got := sh(t, "docker", "exec", name, "cat", file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if exec.Command("docker", "exec", name, "test", "-e", "/named/new.txt").Run() == nil {
		t.Error("a file made after the backup survived the restore")
	}
	if out := sh(t, "docker", "ps", "-a", "--filter", "name="+name+"-bosun-restore", "-q"); out != "" {
		t.Errorf("restore helper left behind: %s", out)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -tags integration -count=1 -v -run TestBackupAndRestoreData ./internal/gate/`
Expected: PASS. The first run builds Bosun's image (about a minute).

Then run the whole suite once: `go test -tags integration -count=1 ./internal/gate/`
Expected: PASS, 6 integration tests.

If the helper cannot read the host backup folder (permission denied inside the helper), check the folder is under a path Docker shares (`/private/var/folders` and `/Users` are shared on OrbStack and Docker Desktop). Test-only fixes are fine; record them.

- [ ] **Step 3: Clean up and commit**

Remove the test image: `docker image rm bosun-it-helper:latest`. Leave no `bosun-it-*` containers or volumes.

```bash
git add internal/gate/integration_test.go
git commit -m "Add real-Docker backup and restore test"
```

---

### Task 8: Image, compose, README, and smoke test

**Files:**
- Modify: `Dockerfile`, `compose.yml`, `README.md`

- [ ] **Step 1: Dockerfile**

In the build stage, extend the `RUN` line with `&& mkdir -m 0700 -p /out/var/lib/bosun-backups`. In the final stage, after the `/var/lib/bosun` copy, add:

```dockerfile
# Gate-only backups; never given to the updater.
COPY --from=build --chown=65532:65532 /out/var/lib/bosun-backups /var/lib/bosun-backups
```

- [ ] **Step 2: compose.yml**

Add `- bosun-backups:/var/lib/bosun-backups` under the service's `volumes` (after `bosun-state`), and `bosun-backups:` under the top-level `volumes`.

- [ ] **Step 3: README.md**

1. Add a row to the "Watch a container" label table:

```markdown
| `bosun.backup=true` | off | Copy the container's writable volumes before each update. |
```

2. Add rows to the "Settings" table:

```markdown
| `BOSUN_BACKUP_DIR` | `/var/lib/bosun-backups` | Backup folder inside the gate (the `bosun-backups` volume). |
| `BOSUN_BACKUP_WARN_SIZE` | `10GB` | A backup bigger than this adds a one-time warning to the update note. |
```

3. Add a section after "Settings":

````markdown
## Backups

With `bosun.backup=true`, Bosun stops the app, copies each writable volume and host
folder with Docker's own copy, then updates. The app stays stopped during the copy,
so big volumes mean long downtime. Bosun keeps one backup per container, in the
`bosun-backups` volume, which only the gate can reach.

To put the data back together with the old version:

```bash
docker exec -it bosun-gate bosun rollback postgres --with-data
```

Data written since the backup is lost. The restore runs a short helper container
from Bosun's own image. It runs as root, because it must delete files and keep file
owners, but it has no network, a read-only root, only three powers (`CHOWN`,
`DAC_OVERRIDE`, `FOWNER`), and it only sees that app's folders. Don't close the
terminal during a restore.

To keep backups in a host folder instead, mount it at `/var/lib/bosun-backups` and
give it to Bosun's user first: `sudo chown 65532:65532 /srv/bosun-backups`.
````

4. In "Limits", replace the line "A rollback swaps the image only. It does not undo data changes." with "A rollback swaps the image only, unless you use `--with-data` with a backup."

- [ ] **Step 4: Smoke test on this Mac**

Docker (OrbStack) runs here with other containers. Only touch `bosun-gate`, `bosun-updater`, `bosun-smoke`, the `bosun-smoke-data` volume, and this compose project.

```bash
export DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g /s) && docker compose up -d --build
```

```bash
docker run -d --name bosun-smoke --label bosun.enable=true --label bosun.backup=true -v bosun-smoke-data:/data nginx:alpine
```

```bash
docker exec -it bosun-gate bosun status
```

Expected: `bosun-smoke` shows `on, none yet` in the BACKUP column.

```bash
docker inspect bosun-updater --format '{{json .Mounts}}'
```

Expected: no `bosun-backups`, no `/var/lib/bosun`, no `docker.sock`.

```bash
docker exec bosun-gate bosun help rollback
```

Expected: the help mentions `--with-data`.

Clean up:

```bash
docker compose down -v
```

```bash
docker rm -f bosun-smoke && docker volume rm bosun-smoke-data
```

- [ ] **Step 5: Commit**

```bash
git add Dockerfile compose.yml README.md
git commit -m "Add the backup volume and backup docs"
```
