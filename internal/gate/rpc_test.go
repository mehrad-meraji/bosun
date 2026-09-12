package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehrad-meraji/bosun/internal/sock"
	"github.com/mehrad-meraji/bosun/internal/state"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		req UpdateRequest
		ok  bool
	}{
		{UpdateRequest{Name: "web", Digest: goodDigest}, true},
		{UpdateRequest{Name: "my_app.v2-x", Digest: goodDigest}, true},
		{UpdateRequest{Name: "../etc", Digest: goodDigest}, false},
		{UpdateRequest{Name: "a b", Digest: goodDigest}, false},
		{UpdateRequest{Name: "", Digest: goodDigest}, false},
		{UpdateRequest{Name: "web", Digest: "sha256:XYZ"}, false},
		{UpdateRequest{Name: "web", Digest: "md5:" + strings.Repeat("a", 64)}, false},
	} {
		err := tc.req.Validate()
		var ref *RefusedError
		if tc.ok != (err == nil) || (err != nil && !errors.As(err, &ref)) {
			t.Errorf("Validate(%+v) = %v, want ok=%v and a RefusedError on failure", tc.req, err, tc.ok)
		}
	}
}

func TestDecodeRefusesUnknownFields(t *testing.T) {
	var r UpdateRequest
	err := decode(strings.NewReader(`{"name":"web","digest":"`+goodDigest+`","auth":"","image":"evil:latest"}`), &r)
	var ref *RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("want a refusal for an unknown field, got %v", err)
	}
}

func TestDecodeRefusesTwoObjects(t *testing.T) {
	var r UpdateRequest
	if err := decode(strings.NewReader(`{"name":"a"} {"name":"b"}`), &r); err == nil {
		t.Fatal("want a refusal for two objects")
	}
}

// A bad request is refused before Docker is touched. Gate.D is nil here, so
// any Docker call would panic.
func TestServeRefusesBadUpdate(t *testing.T) {
	dir, err := os.MkdirTemp("", "bg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "g.sock")
	l, err := sock.Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Gate{Dir: dir}).Serve(ctx, l)

	_, err = Dial(p).Update(ctx, "web", "sha256:nope", "")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want refused, got %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

func TestServeSkipClear(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	setEntry(t, g, "app", state.Entry{Skip: []string{"sha256:d2"}})
	c, stop := serveGate(t, g)
	defer stop()

	res, err := c.SkipClear(context.Background(), "app")
	if err != nil || res.Cleared != 1 {
		t.Fatalf("SkipClear = %+v, %v, want 1 cleared", res, err)
	}
	if _, err := c.SkipClear(context.Background(), "../etc"); !IsRefused(err) {
		t.Errorf("err = %v, want the client to see a refusal", err)
	}
}

// A busy gate must read as busy through the socket too, not as a failure:
// the control server drops a failed command but retries a busy one.
func TestServeSkipClearIsBusyOverTheSocket(t *testing.T) {
	f := swapFake(true)
	g := f.gate(t)
	lock, _, err := state.Open(g.Dir, true) // hold the lock, like a running update
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	c, stop := serveGate(t, g)
	defer stop()

	_, err = c.SkipClear(context.Background(), "app")
	if !IsBusy(err) {
		t.Errorf("err = %v, want IsBusy", err)
	}
}

// serveGate runs g on a socket and returns a client for it.
// ponytail: os.MkdirTemp, not t.TempDir, because macOS caps socket paths at 104 bytes.
func serveGate(t *testing.T, g *Gate) (*Client, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bg")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "g.sock")
	l, err := sock.Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = g.Serve(ctx, l) }()
	return Dial(p), func() { cancel(); os.RemoveAll(dir) }
}

func FuzzUpdateRequest(f *testing.F) {
	f.Add([]byte(`{"name":"web","digest":"` + goodDigest + `","auth":"eyJ1c2VybmFtZSI6InUifQ=="}`))
	f.Add([]byte(`{"name":"web","digest":null}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r UpdateRequest
		if decode(bytes.NewReader(b), &r) != nil || r.Validate() != nil {
			return
		}
		if !nameRE.MatchString(r.Name) || !digestRE.MatchString(r.Digest) {
			t.Fatalf("accepted a bad request: %+v", r)
		}
	})
}
