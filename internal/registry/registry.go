// Package registry asks registries for the current digest of a tag, and
// builds the login Docker needs to pull it.
package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Checker struct {
	insecure  map[string]bool
	transport http.RoundTripper
	keychain  authn.Keychain
}

// New makes a checker. insecure lists registries allowed over plain HTTP.
// caFile, if set, adds a CA for registries with private certificates.
// Logins come from $DOCKER_CONFIG/config.json.
func New(insecure []string, caFile string) (*Checker, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	set := map[string]bool{}
	for _, r := range insecure {
		set[r] = true
	}
	return &Checker{insecure: set, transport: t, keychain: authn.DefaultKeychain}, nil
}

func (c *Checker) parse(ref string) (name.Reference, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	if c.insecure[r.Context().RegistryStr()] {
		return name.ParseReference(ref, name.Insecure)
	}
	return r, nil
}

// Digest returns the registry's current digest for ref's tag. It uses HEAD,
// which does not count against Docker Hub pull limits.
func (c *Checker) Digest(ctx context.Context, ref string) (string, error) {
	r, err := c.parse(ref)
	if err != nil {
		return "", err
	}
	d, err := remote.Head(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain), remote.WithTransport(c.transport))
	if err != nil {
		return "", err
	}
	return d.Digest.String(), nil
}

// Auth returns the X-Registry-Auth value for pulling ref, or "" for anonymous.
func (c *Checker) Auth(ref string) (string, error) {
	r, err := c.parse(ref)
	if err != nil {
		return "", err
	}
	a, err := c.keychain.Resolve(r.Context())
	if err != nil {
		return "", err
	}
	if a == authn.Anonymous {
		return "", nil
	}
	ac, err := a.Authorization()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]string{
		"username":      ac.Username,
		"password":      ac.Password,
		"identitytoken": ac.IdentityToken,
		"serveraddress": r.Context().RegistryStr(),
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
