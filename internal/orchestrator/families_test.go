package orchestrator

import (
	"testing"

	"github.com/traumventures/offshore/internal/scheduler"
)

// Worktrees seen with a family reach the scheduler, most recent last,
// and survive a restart (families.json).
func TestFamiliesReachScheduler(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	o.mu.Lock()
	o.noteFamilyLocked("fam", "a")
	o.noteFamilyLocked("fam", "b")
	o.noteFamilyLocked("fam", "a")
	o.noteFamilyLocked("", "c")
	var p scheduler.Pool
	o.schedPoolLocked(&p)
	o.mu.Unlock()
	if got := p.Families["fam"]; len(got) != 2 || got[0] != "b" || got[1] != "a" || len(p.Families) != 1 {
		t.Fatalf("families %v", p.Families)
	}
	o.loadFamilies()
	if got := o.families["fam"]; len(got) != 2 {
		t.Fatalf("after reload %v", o.families)
	}
}
