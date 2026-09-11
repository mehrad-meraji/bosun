package sock

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Stopping the gate must not cut off a running swap: Serve waits for the
// call to finish, and the caller still gets its answer.
func TestServeWaitsForRunningCalls(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs") // macOS caps socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "s.sock")
	l, err := Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Write([]byte(`"swapped"`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, l, h) }()

	reply := make(chan error, 1)
	var got string
	go func() { reply <- Post(context.Background(), Client(p), "http://x/", struct{}{}, &got) }()
	<-started
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned while a call was running: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-reply; err != nil || got != "swapped" {
		t.Fatalf("call = %q %v, want it to finish", got, err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}
