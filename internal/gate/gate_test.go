package gate

import (
	"testing"
	"time"

	"github.com/mehrad-meraji/bosun/internal/docker"
)

func ctr(running bool, restarts int, health string) *docker.Container {
	c := &docker.Container{RestartCount: restarts}
	c.State.Running = running
	c.State.Status = "running"
	if health != "" {
		c.State.Health = &docker.Health{Status: health}
	}
	return c
}

func TestVerdict(t *testing.T) {
	const timeout = 10 * time.Second
	for _, tc := range []struct {
		name    string
		c       *docker.Container
		elapsed time.Duration
		done    bool
		ok      bool
	}{
		{"no healthcheck, still waiting", ctr(true, 0, ""), time.Second, false, false},
		{"no healthcheck, stable until timeout", ctr(true, 0, ""), timeout, true, true},
		{"exited", ctr(false, 0, ""), time.Second, true, false},
		{"restarted once", ctr(true, 1, ""), time.Second, true, false},
		{"healthcheck starting", ctr(true, 0, "starting"), time.Second, false, false},
		{"healthy", ctr(true, 0, "healthy"), time.Second, true, true},
		{"unhealthy", ctr(true, 0, "unhealthy"), time.Second, true, false},
		{"never healthy", ctr(true, 0, "starting"), timeout, true, false},
	} {
		done, err := verdict(tc.c, tc.elapsed, timeout)
		if done != tc.done || (done && (err == nil) != tc.ok) {
			t.Errorf("%s: done=%v err=%v; want done=%v ok=%v", tc.name, done, err, tc.done, tc.ok)
		}
	}
}

func TestTrackable(t *testing.T) {
	for ref, want := range map[string]bool{
		"nginx:1.27":            true,
		"ghcr.io/me/app":        true,
		"sha256:abcdef":         false,
		"nginx@sha256:abcdef":   false,
		"nginx:1.27@sha256:abc": false,
		"":                      false,
	} {
		c := &docker.Container{}
		c.Config.Image = ref
		if _, ok := trackable(c); ok != want {
			t.Errorf("trackable(%q) = %v, want %v", ref, ok, want)
		}
	}
}

func TestDigestsOf(t *testing.T) {
	img := &docker.Image{RepoDigests: []string{"nginx@sha256:aa", "localhost:5000/nginx@sha256:bb"}}
	got := digestsOf(img)
	if len(got) != 2 || got[0] != "sha256:aa" || got[1] != "sha256:bb" {
		t.Errorf("digestsOf = %v", got)
	}
}

func TestTimeoutOf(t *testing.T) {
	c := &docker.Container{}
	if timeoutOf(c) != 60*time.Second {
		t.Error("default must be 60s")
	}
	c.Config.Labels = map[string]string{LabelTimeout: "2m"}
	if timeoutOf(c) != 2*time.Minute {
		t.Error("label must win")
	}
	c.Config.Labels[LabelTimeout] = "soon"
	if timeoutOf(c) != 60*time.Second {
		t.Error("bad label must fall back to 60s")
	}
}
