package scheduler

import (
	"testing"
	"time"
)

// A new worktree's first build has no runs of its own: its run times and
// read set come from the same command in sibling repositories, not the
// 2-minute default (production: 3 m planned, 4.5-6 m real).
func TestProfileFallsBackToPeerKeys(t *testing.T) {
	h := NewHistory("")
	for _, repo := range []string{"a", "b", "c"} {
		h.Observe(repo+"/cargo/check?p=api", Usage{Wall: 5 * time.Minute, ExitCode: 0, Provider: "aws"})
		h.Observe(repo+"/cargo/check?p=api", Usage{Wall: 20 * time.Second, Warm: true, ReadBytes: 3 << 30})
	}
	h.Observe("b/cargo/check?p=api", Usage{Wall: 6 * time.Second, ExitCode: 101, Provider: "aws"}) // early compile error
	p := h.Profile("new/cargo/check?p=api")
	if p.ColdRun("aws") != 5*time.Minute || p.WarmRun() != 20*time.Second || p.ReadSet != 3<<30 {
		t.Fatalf("peer profile: cold %v warm %v read %d", p.ColdRun("aws"), p.WarmRun(), p.ReadSet)
	}
	// The key's own runs win; a parent key of the same repository comes
	// before peers.
	h.Observe("new/cargo/check", Usage{Wall: 7 * time.Minute, Provider: "aws"})
	if p := h.Profile("new/cargo/check?p=api"); p.ColdRun("aws") != 7*time.Minute {
		t.Fatalf("parent key: %v", p.ColdRun("aws"))
	}
	// Failed runs do not count as run times while successful ones exist.
	if p := profileOf(h.runs["b/cargo/check?p=api"]); p.ColdRun("aws") != 5*time.Minute {
		t.Fatalf("failed run counted: %v", p.ColdRun("aws"))
	}
}
