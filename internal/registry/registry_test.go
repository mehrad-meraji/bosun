package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestDigest(t *testing.T) {
	// Keep this Mac's own Docker logins (and keychain helpers) out of the test.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	srv := httptest.NewServer(ggcrregistry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	ref := u.Host + "/app:1"

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	parsedRef, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsedRef, img); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()

	c, err := New(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Digest(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Fatalf("Digest = %s, want %s", got, want)
	}
}

func TestAuthAnonymous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	c, _ := New(nil, "")
	a, err := c.Auth("example.com/app:1")
	if err != nil || a != "" {
		t.Fatalf("Auth = %q, %v; want empty", a, err)
	}
}

func TestAuthFromDockerConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	cfg := `{"auths":{"example.com":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:p")) + `"}}}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600)

	c, _ := New(nil, "")
	a, err := c.Auth("example.com/app:1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(a)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	json.Unmarshal(raw, &got)
	if got["username"] != "u" || got["password"] != "p" || got["serveraddress"] != "example.com" {
		t.Fatalf("auth = %v", got)
	}
}

func TestBadCAFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(p, []byte("not a cert"), 0o600)
	if _, err := New(nil, p); err == nil {
		t.Fatal("want an error for a CA file with no certificates")
	}
}
