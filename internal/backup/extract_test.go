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
