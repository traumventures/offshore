package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
)

func TestGroupsByLabel(t *testing.T) {
	capa := &scheduler.Resources{CPU: 4, Memory: 8 * gi}
	res := &scheduler.Resources{CPU: 2, Memory: 4 * gi} // half the machine
	codexA := map[string]string{"agent": "codex", "worktree": "/w/a", "branch": "wp/a"}
	codexB := map[string]string{"agent": "codex", "worktree": "/w/b", "branch": "wp/b", "purpose": "prewarm"}
	claude := map[string]string{"agent": "claude-code", "worktree": "/w/a", "branch": "wp/a"}
	end := func(h float64, l map[string]string, wall time.Duration, cpuStall float64) events.Event {
		return events.Event{Type: events.TaskEnd, Time: at(h), Repo: "r", Price: 1, Reserve: res, Capacity: capa, Labels: l,
			Usage: &scheduler.Usage{Wall: wall, CPUSeconds: wall.Seconds(), CPUStall: cpuStall}}
	}
	evs := []events.Event{
		{Type: events.TaskPlaced, Time: at(1), Placement: "assign", WaitMs: 2000, Labels: codexA},
		{Type: events.TaskPlaced, Time: at(1), Placement: "assign", WaitMs: 4000, Labels: codexB},
		{Type: events.TaskPlaced, Time: at(1), Placement: "timeout", Labels: claude},
		{Type: events.TaskPlaced, Time: at(1), Placement: "assign", WaitMs: 500, Labels: claude},
		{Type: events.TaskPlaced, Time: at(1), Placement: "assign", WaitMs: 100, Repo: "old"}, // no labels
		end(2, codexA, time.Hour, 360),
		end(3, codexB, 30*time.Minute, 0),
		end(4, claude, 2*time.Hour, 0),
		end(-1, codexA, time.Hour, 0), // outside the window
	}
	from, to := at(0), at(10)
	g := Groups(evs, from, to, "agent")
	if len(g.Groups) != 3 || g.Groups[0].Key != "claude-code" || g.Groups[1].Key != "codex" || g.Groups[2].Key != "(human)" {
		t.Fatalf("agent groups %+v", g.Groups)
	}
	codex := g.Groups[1]
	// 1.5 session-hours at $1/h × 0.5 share; wait p50 2s, max 4s.
	if codex.Sessions != 2 || codex.Prewarms != 1 || !near(codex.SessionHours, 1.5) || !near(codex.Cost, 0.75) ||
		codex.WaitMax != 4*time.Second || codex.Requests != 2 || !near(codex.CPUStallShare, 360.0/5400) {
		t.Fatalf("codex %+v", codex)
	}
	if cl := g.Groups[0]; cl.Failed != 1 || cl.Requests != 2 || !near(cl.Cost, 1) {
		t.Fatalf("claude %+v", cl)
	}
	w := Groups(evs, from, to, "worktree")
	if len(w.Groups) != 3 || w.Groups[0].Key != "/w/a" || w.Groups[0].Sessions != 2 || w.Groups[2].Key != "old" {
		t.Fatalf("worktree groups %+v", w.Groups)
	}
	r := Compute(evs, from, to)
	if len(r.Groupings) != 2 || r.Groupings[0].By != "agent" || r.Groupings[1].By != "worktree" {
		t.Fatalf("default groupings %+v", r.Groupings)
	}
	var b bytes.Buffer
	r.Write(&b)
	if !strings.Contains(b.String(), "by agent") || !strings.Contains(b.String(), "1 prewarm") {
		t.Fatalf("report text:\n%s", b.String())
	}
}
