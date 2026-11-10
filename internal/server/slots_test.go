package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSlotsExclusiveAndWarm(t *testing.T) {
	s, err := newSlots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := s.lease("repo", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.lease("repo", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("concurrent leases share a slot")
	}
	os.WriteFile(filepath.Join(a, "data", "artifact"), []byte("x"), 0o644)
	s.release(a)
	time.Sleep(1100 * time.Millisecond) // last_used has second resolution
	s.release(b)
	// The most recently released slot is reused first.
	c, _, _ := s.lease("repo", time.Hour)
	if c != b {
		t.Fatalf("got %s, want most recently used %s", c, b)
	}
	d, _, _ := s.lease("repo", time.Hour)
	if d != a {
		t.Fatalf("got %s, want %s", d, a)
	}
	if _, err := os.Stat(filepath.Join(d, "data", "artifact")); err != nil {
		t.Fatal("warm slot lost its data")
	}
}

func TestSlotsExpire(t *testing.T) {
	s, err := newSlots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.lease("repo", time.Hour)
	os.WriteFile(filepath.Join(a, "data", "artifact"), []byte("x"), 0o644)
	s.release(a)
	// Age the slot beyond the TTL.
	m, _ := readMeta(a)
	m.Created = time.Now().Add(-25 * time.Hour).Unix()
	writeMeta(a, m)
	if got := s.warmRepos(24 * time.Hour); len(got) != 0 {
		t.Fatalf("expired slot reported warm: %v", got)
	}
	b, _, _ := s.lease("repo", 24*time.Hour)
	if _, err := os.Stat(filepath.Join(b, "data", "artifact")); err == nil {
		t.Fatal("expired build data was reused")
	}
	s.release(b)
	m, _ = readMeta(b)
	m.Created = time.Now().Add(-25 * time.Hour).Unix()
	writeMeta(b, m)
	if n := s.gc(24 * time.Hour); n != 1 {
		t.Fatalf("gc removed %d slots, want 1", n)
	}
}

func TestExpandPath(t *testing.T) {
	env := []string{"HOME=/root", "CARGO_HOME=/usr/local/cargo"}
	if got := expandPath("$CARGO_HOME/registry", env); got != "/usr/local/cargo/registry" {
		t.Fatal(got)
	}
	if got := expandPath("~/.cache/x", env); got != "/root/.cache/x" {
		t.Fatal(got)
	}
}
