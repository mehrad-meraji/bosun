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
