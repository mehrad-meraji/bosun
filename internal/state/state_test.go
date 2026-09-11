package state

import (
	"errors"
	"testing"
	"time"
)

func TestSaveAndRead(t *testing.T) {
	dir := t.TempDir()
	f, s, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Entry("web")
	e.Prev = "bosun/prev/web:abc"
	e.Downtime = 2 * time.Second
	e.AddSkip("sha256:1")
	e.AddSkip("sha256:1")
	s.Pending = append(s.Pending, Pending{Name: "web", OldID: "id1", TmpName: "web-bosun-aa"})
	s.AddEvent("recovered", "web", "old version restored")
	if err := f.Save(s); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := got.Containers["web"]
	if g.Prev != e.Prev || g.Downtime != e.Downtime || len(g.Skip) != 1 {
		t.Errorf("entry = %+v, want %+v with one skip", g, e)
	}
	if len(got.Pending) != 1 || got.Pending[0].TmpName != "web-bosun-aa" {
		t.Errorf("pending = %+v", got.Pending)
	}
	if len(got.Events) != 1 || got.Events[0].Message != "old version restored" {
		t.Errorf("events = %+v", got.Events)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	s, err := Read(t.TempDir())
	if err != nil || s.Containers == nil || len(s.Containers) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestOpenNoWaitIsBusy(t *testing.T) {
	dir := t.TempDir()
	f, _, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := Open(dir, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}
