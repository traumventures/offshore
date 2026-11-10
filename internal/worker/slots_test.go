package worker

import (
	"github.com/traumventures/cloudsourced/internal/clock"
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
	clk := clock.NewFake(time.Now())
	s.clock = clk
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
	clk.Advance(2 * time.Second) // last_used has second resolution
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
	clk := clock.NewFake(time.Now())
	s.clock = clk
	a, _, _ := s.lease("repo", time.Hour)
	os.WriteFile(filepath.Join(a, "data", "artifact"), []byte("x"), 0o644)
	s.release(a)
	// The slot ages beyond the TTL.
	clk.Advance(25 * time.Hour)
	if got := s.warmRepos(24 * time.Hour); len(got) != 0 {
		t.Fatalf("expired slot reported warm: %v", got)
	}
	b, _, _ := s.lease("repo", 24*time.Hour)
	if _, err := os.Stat(filepath.Join(b, "data", "artifact")); err == nil {
		t.Fatal("expired build data was reused")
	}
	s.release(b)
	clk.Advance(25 * time.Hour)
	if n := s.gc(24 * time.Hour); n != 1 {
		t.Fatalf("gc removed %d slots, want 1", n)
	}
}

// Repositories with different build TTLs share a machine: each slot expires
// under its own repository's TTL, bounded by the machine TTL.
func TestSlotsPerRepositoryTTL(t *testing.T) {
	s, err := newSlots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	machine := 24 * time.Hour
	short, _, _ := s.lease("short", time.Hour)
	long, _, _ := s.lease("long", 12*time.Hour)
	s.release(short)
	s.release(long)
	for _, p := range []string{short, long} {
		m, _ := readMeta(p)
		m.Created = time.Now().Add(-2 * time.Hour).Unix()
		writeMeta(p, m)
	}
	if got := s.warmRepos(machine); len(got) != 1 || got[0] != "long" {
		t.Fatalf("warm repos %v, want [long]", got)
	}
	if _, err := s.latest("short", machine); err == nil {
		t.Fatal("slot past its repository TTL still served")
	}
	if n := s.gc(machine); n != 1 {
		t.Fatalf("gc removed %d slots, want 1", n)
	}
	if _, err := os.Stat(long); err != nil {
		t.Fatal("slot within its TTL was collected")
	}
	// The machine TTL stays an upper bound.
	if n := s.gc(time.Hour); n != 1 {
		t.Fatalf("gc under a lower machine TTL removed %d, want 1", n)
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
