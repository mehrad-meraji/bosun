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
