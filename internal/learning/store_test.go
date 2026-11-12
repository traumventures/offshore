package learning

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// Usage and refetch factors survive a restart through learning.json;
// nothing is read from the event log.
func TestStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	h := s.History()
	for i := 0; i < 3; i++ {
		h.Observe("r/cargo/build", scheduler.Usage{PeakMemory: 2 << 30, Wall: time.Minute, CPUSeconds: 120})
	}
	r := s.Refetch()
	r.Add("aws", 4)
	s2 := Open(dir)
	if got := s2.History().Profile("r/cargo/build").Runs; got != 3 {
		t.Fatalf("runs after restart: %d", got)
	}
	if f := s2.Refetch().Factors(); f["aws"] != 4 || f[""] != 4 {
		t.Fatalf("refetch after restart: %v", f)
	}
	if n := s2.Refetch().Samples("aws"); n != 1 {
		t.Fatalf("samples: %d", n)
	}
}

// Another schema version is not loaded; the legacy estimator file is
// imported once.
func TestStoreVersionAndLegacy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, legacyUsage), []byte(`{"k":[{"peak_memory":1,"cpu_seconds":1,"wall":1000000000,"oom":false}]}`), 0o600)
	if s := Open(dir); s.Keys() != 1 {
		t.Fatalf("legacy import: %d keys", s.Keys())
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":99,"usage":{"a":[],"b":[]}}`), 0o600)
	if s := Open(dir); s.Keys() != 0 {
		t.Fatalf("other version loaded: %d keys", s.Keys())
	}
}

// The store is bounded: the least recently updated keys go first.
func TestStoreBounded(t *testing.T) {
	s := Open("")
	clock := time.Unix(0, 0)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < MaxKeys+10; i++ {
		s.setUsage(fmt.Sprintf("k%d", i), []scheduler.Usage{{Wall: time.Second}})
	}
	if s.Keys() != MaxKeys {
		t.Fatalf("keys: %d", s.Keys())
	}
	s.mu.Lock()
	_, old := s.d.Usage["k0"]
	_, recent := s.d.Usage[fmt.Sprintf("k%d", MaxKeys+9)]
	s.mu.Unlock()
	if old || !recent {
		t.Fatal("evicted the wrong keys")
	}
}
