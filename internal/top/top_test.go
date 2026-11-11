package top

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

const gib = int64(1) << 30

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func fakeSnapshot(cpuSec float64, at time.Time) Snapshot {
	m := orchestrator.Machine{
		Machine: provider.Machine{ID: "m-aws-1", State: provider.Running, CPU: 32, Memory: 64 * gib, EnvHash: "e1",
			Labels: map[string]string{provider.LabelSize: "c7a.8xlarge"}},
		Provider: "aws", PricePerHour: 1.5,
		Status: &proto.Status{Ready: true, CPU: 32, Memory: 64 * gib, UsedCPU: 12,
			Measured: scheduler.Resources{CPU: 10, Memory: 20 * gib}, Pressure: scheduler.Pressure{CPU: 0.12},
			Sessions: []proto.Session{
				{ID: "s1", Repo: "/home/u/app", Argv: []string{"cargo", "build"}, Started: at.Add(-90 * time.Second).Unix(),
					CPU: 8, Memory: 16 * gib, UsedMemory: 12 * gib, CPUSeconds: cpuSec,
					Labels: map[string]string{"agent": "codex", "worktree": "/home/u/cs-wt/feature-a", "branch": "wp/a"}},
				{ID: "s2", Repo: "/home/u/app", Argv: []string{"cargo", "test"}, Started: at.Add(-10 * time.Minute).Unix(),
					CPU: 4, Memory: 8 * gib, UsedMemory: 2 * gib, CPUSeconds: 60,
					Labels: map[string]string{"agent": "claude-code", "worktree": "/home/u/app", "branch": "main"}},
			}},
	}
	stopped := orchestrator.Machine{Machine: provider.Machine{ID: "m-aws-2", State: provider.Stopped, CPU: 16, Memory: 32 * gib}, Provider: "aws"}
	gone := orchestrator.Machine{Machine: provider.Machine{ID: "m-old"}, Removed: at.Add(-time.Hour)}
	return Snapshot{Time: at, Machines: []orchestrator.Machine{m, stopped, gone}, QueueKnown: true,
		Queue: []orchestrator.QueuedTask{{ID: "q1", Since: at.Add(-5 * time.Second), RepoID: "r", Key: "r/cargo/check",
			Need: scheduler.Resources{CPU: 2, Memory: 4 * gib}, Labels: map[string]string{"agent": "codex", "worktree": "/home/u/cs-wt/b"}}},
		Cost: orchestrator.CostReport{PerHour: 1.5, Total: 7.25}}
}

func TestOnceFrame(t *testing.T) {
	homeDir = "/home/u"
	h := NewHistory(0)
	h.Seed([]events.Event{
		{Type: events.MachineSample, Time: now.Add(-5 * time.Minute), Machine: "m-aws-1", Price: 1.5,
			Capacity: &scheduler.Resources{CPU: 32, Memory: 64 * gib}, Used: &scheduler.Resources{CPU: 30, Memory: 40 * gib}, Sessions: 3},
	})
	s0 := fakeSnapshot(100, now.Add(-2*time.Second))
	s := fakeSnapshot(108, now)
	var r Rates
	r.Update(s0)
	rates := r.Update(s)
	if rates["s1"] != 4 { // 8 CPU seconds in 2 s
		t.Fatalf("rate %v", rates)
	}
	h.Add(s0)
	h.Add(s)
	v := DefaultView()
	v.Width = 160
	out := Frame(s, h, v, rates, PlainStyles())
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain frame has escape codes:\n%q", out)
	}
	for _, want := range []string{
		"machines 2 (1 up)", "sessions 2", "queue 1", "$1.50/h now", "$7.25 today",
		"CPU busy 10.0 / 32 cores", "Memory 20.0G / 64.0G", "Sessions 2  queue 1",
		"m-aws-1", "aws/c7a.8xlarge", "10.0/32", "psi 0.12/0.00", "2 sess", "m-aws-2",
		"codex", "claude-code", "~/cs-wt/feature-a", "wp/a", "4.0/8.0", "12.0G/16.0G", "cargo build", "1m30s",
		"Queue (1)", "q1", "~/cs-wt/b", "r/cargo/check",
		"█", "window 10m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	if strings.Contains(out, "m-old") {
		t.Error("removed machine shown")
	}
	// Default sort: longest running first.
	if strings.Index(out, "cargo test") > strings.Index(out, "cargo build") {
		t.Errorf("sessions not sorted by elapsed:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := len([]rune(line)); w > 160 {
			t.Errorf("line wider than 160 (%d): %q", w, line)
		}
	}
	t.Log("\n" + out)
}

func TestSortAndKeys(t *testing.T) {
	s := fakeSnapshot(108, now)
	m := &model{snap: s, have: true, hist: NewHistory(0), view: DefaultView(), styles: PlainStyles(),
		cur: map[string]float64{"s1": 4, "s2": 0.1}}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.key("c") // busy CPU descending
	rows := Rows(s, m.cur)
	SortRows(rows, m.view.Sort, m.view.Asc)
	if rows[0].Session != "s1" {
		t.Fatalf("cpu sort %+v", rows)
	}
	m.key("c") // toggles to ascending
	if !m.view.Asc {
		t.Fatal("second press must reverse")
	}
	m.key("a")
	SortRows(rows, m.view.Sort, m.view.Asc)
	if m.view.Sort != SortAgent || !m.view.Asc || rows[0].Agent != "claude-code" {
		t.Fatalf("agent sort %+v %+v", m.view, rows)
	}
	m.key("down")
	m.key("down") // clamped to the last row
	if m.view.Selected != 1 {
		t.Fatalf("selected %d", m.view.Selected)
	}
	m.key("+")
	if m.view.Window != 5*time.Minute {
		t.Fatalf("zoom in: %v", m.view.Window)
	}
	m.key("-")
	m.key("-")
	if m.view.Window != 30*time.Minute {
		t.Fatalf("zoom out: %v", m.view.Window)
	}
	if !strings.Contains(m.View(), "AGENT▲") {
		t.Fatalf("sort marker missing:\n%s", m.View())
	}
	if lines := strings.Count(m.View(), "\n") + 1; lines > 40 {
		t.Fatalf("frame has %d lines for a 40 line terminal", lines)
	}
	if m.key("q") == nil {
		t.Fatal("q must quit")
	}
}

func TestSparklineAndSeries(t *testing.T) {
	pts := []Point{{T: now.Add(-50 * time.Second), BusyCPU: 4}, {T: now.Add(-10 * time.Second), BusyCPU: 8}}
	vals, ok := Series(pts, func(p Point) time.Time { return p.T }, func(p Point) float64 { return p.BusyCPU }, now, time.Minute, 6)
	if ok[0] || !ok[1] || vals[1] != 4 || vals[2] != 4 || vals[5] != 8 {
		t.Fatalf("series %v %v", vals, ok)
	}
	if sp := Sparkline(vals, ok, 8, 2); sp != "     █\n █████" {
		t.Fatalf("sparkline %q", sp)
	}
	if sp := Sparkline([]float64{0, 1}, []bool{true, true}, 16, 1); sp != "▁▁" {
		t.Fatalf("low values keep a baseline: %q", sp)
	}
}

func TestMachineCacheColumns(t *testing.T) {
	s := fakeSnapshot(100, now)
	s.Machines[0].Status.VM = &proto.VMStats{Capacity: 50 * gib, ResidentBytes: 40 * gib, DirtyBytes: gib / 2,
		WorkingSetBytes: 60 * gib, FaultRate: 12.5, Thrashing: true}
	v := DefaultView()
	v.Width = 260
	out := Frame(s, NewHistory(0), v, nil, PlainStyles())
	for _, want := range []string{"cache 40.0G/50.0G", "dirty 512M", "ws 60.0G", "12.5 flt/s", "THRASH"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Narrow frames drop the cache columns before the rest.
	v.Width = 120
	if out := Frame(s, NewHistory(0), v, nil, PlainStyles()); strings.Contains(out, "THRASH") || !strings.Contains(out, "m-aws-1") {
		t.Errorf("narrow frame:\n%s", out)
	}
}

// Graphs count only active machines: a stopped machine adds no capacity,
// and two samples of one machine in a bucket count once.
func TestGraphsOnlyActiveMachines(t *testing.T) {
	h := NewHistory(0)
	h.Add(fakeSnapshot(100, now))
	if p := h.Pool[len(h.Pool)-1]; p.CapCPU != 32 || p.CapMem != 64*gib || p.CostPerHour != 1.5 {
		t.Fatalf("live point counts stopped machines: %+v", p)
	}
	h2 := NewHistory(0)
	c := &scheduler.Resources{CPU: 16, Memory: 30 * gib}
	h2.Seed([]events.Event{
		{Type: events.MachineSample, Time: now.Add(-60 * time.Second), Machine: "a", Capacity: c, Price: 1},
		{Type: events.MachineSample, Time: now.Add(-52 * time.Second), Machine: "a", Capacity: c, Price: 1},
		{Type: events.MachineSample, Time: now.Add(-55 * time.Second), Machine: "b", Capacity: c, Price: 1},
	})
	if len(h2.Pool) != 1 || h2.Pool[0].CapMem != 60*gib || h2.Pool[0].CostPerHour != 2 {
		t.Fatalf("seeded %+v", h2.Pool)
	}
}
