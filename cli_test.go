package main

import (
	"reflect"
	"testing"
	"time"
)

func TestSplitArgs(t *testing.T) {
	pos, flags := splitArgs([]string{"nginx", "--yes", "--dry-run"})
	if !reflect.DeepEqual(pos, []string{"nginx"}) || !flags["yes"] || !flags["dry-run"] {
		t.Fatalf("got %v %v", pos, flags)
	}
	if err := checkFlags(flags, "yes"); err == nil {
		t.Fatal("want an error for --dry-run when only --yes is allowed")
	}
}

func TestAgo(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{72 * time.Hour, "3 days ago"},
	} {
		if got := ago(time.Now().Add(-tc.d)); got != tc.want {
			t.Errorf("ago(-%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if ago(time.Time{}) != "never" {
		t.Error("zero time must be never")
	}
}
