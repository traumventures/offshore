package top

import (
	"fmt"
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
					Labels: map[string]string{"agent": "codex", "user": "u", "host": "box", "worktree": "/home/u/cs-wt/feature-a", "branch": "wp/a"}},
				{ID: "s2", Repo: "/home/u/app", Argv: []string{"cargo", "test"}, Started: at.Add(-10 * time.Minute).Unix(),
					CPU: 4, Memory: 8 * gib, UsedMemory: 2 * gib, CPUSeconds: 60,
					Labels: map[string]string{"agent": "claude-code", "worktree": "/home/u/app", "branch": "main"}},
			}},
	}
	stopped := orchestrator.Machine{Machine: provider.Machine{ID: "m-aws-2", State: provider.Stopped, CPU: 16, Memory: 32 * gib}, Provider: "aws"}
	gone := orchestrator.Machine{Machine: provider.Machine{ID: "m-old"}, Removed: at.Add(-time.Hour)}
	return Snapshot{Time: at, Machines: []orchestrator.Machine{m, stopped, gone}, QueueKnown: true,
		Queue: []orchestrator.QueuedTask{{ID: "q1", Since: at.Add(-5 * time.Second), RepoID: "r", Key: "r/cargo/check",
			Need: scheduler.Resources{CPU: 2, Memory: 4 * gib}, Labels: map[string]string{"agent": "codex", "worktree": "/home/u/cs-wt/b"}, Queue: "wait: p=0.86"}},
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
		"machines 2 (1 up)", "sessions 2", "queue 1", "$1.50/h now · $7.25 today",
		"CPU busy 10.0 / 32 cores", "Memory 20.0G / 64.0G",
		"MACHINE", "PROVIDER/SIZE", "STATE", "SESS", "$/H",
		"m-aws-1", "aws/c7a.8xlarge", "10.0/32", "20.0G/64.0G", "busy", "m-aws-2", "stopped",
		"USER@HOST", "u@box", "~/cs-wt/feature-a", "wp/a", "4.0/8.0", "12.0G/16.0G", "cargo build", "1m30s",
		"queued (wait: p=0.86)", "q1", "~/cs-wt/b", "cargo check", "need 2.0",
		"█", "window 10m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	for _, bad := range []string{"m-old", "AGENT", "Sessions 2  queue 1", "Cost $", "psi "} {
		if strings.Contains(out, bad) {
			t.Errorf("frame has %q", bad)
		}
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

// Table columns are aligned: every row's cells start at the header's
// column positions, numbers are right-aligned.
func TestTableAlignment(t *testing.T) {
	cols := []Col{{Title: "NAME"}, {Title: "CPU", Bar: true}, {Title: "N", Right: true}, {Title: "CMD", Flex: true, Min: 5}}
	rows := [][]Cell{
		{C("a"), B(1, 4, "1.0/4"), C("7"), C("cargo build --release")},
		{C("longer-name"), B(4, 4, "4.0/16"), C("123"), C("x")},
	}
	ws := layoutCols(cols, rows, 60)
	st := PlainStyles()
	hdr := renderHeader(cols, ws, nil)
	r0, r1 := renderRow(cols, ws, rows[0], st), renderRow(cols, ws, rows[1], st)
	if len([]rune(r0)) != len([]rune(r1)) || len([]rune(r0)) != 60 || len([]rune(hdr)) != 60 {
		t.Fatalf("widths %d %d %d:\n%s\n%s\n%s", len([]rune(hdr)), len([]rune(r0)), len([]rune(r1)), hdr, r0, r1)
	}
	ri := func(s, sub string) int { return len([]rune(s[:strings.Index(s, sub)])) }
	if ri(hdr, "CPU") != ri(r0, "█") || ri(hdr, "CMD") != ri(r0, "cargo") {
		t.Fatalf("columns not aligned:\n%s\n%s", hdr, r0)
	}
	// Right alignment: the values end at the same column.
	if ri(r0, "1.0/4")+len("1.0/4") != ri(r1, "4.0/16")+len("4.0/16") ||
		ri(r0, " 7 ")+2 != ri(r1, "123 ")+3 {
		t.Fatalf("numbers not right-aligned:\n%s\n%s", r0, r1)
	}
	if !strings.Contains(r0, "██░░░░ ") || !strings.Contains(r1, "██████ ") {
		t.Fatalf("bars:\n%s\n%s", r0, r1)
	}
	// Narrow: flex shrinks, then droppable columns go.
	cols[2].Drop = 1
	ws = layoutCols(cols, rows, 30)
	if ws[2] != -1 || ws[3] < 4 {
		t.Fatalf("narrow widths %v", ws)
	}
}

func TestLayoutUsesFullHeight(t *testing.T) {
	for _, tc := range []struct {
		h, machines, storage, sessions int
	}{{24, 3, 2, 5}, {40, 3, 4, 30}, {60, 10, 6, 100}, {80, 2, 0, 2}, {10, 3, 3, 3}, {0, 3, 2, 5}} {
		l := ComputeLayout(tc.h, tc.machines, tc.storage, tc.sessions)
		if l.Graph < 1 || l.Machines < 1 || l.Sessions < 1 || l.Graph > maxGraph {
			t.Fatalf("%+v: %+v", tc, l)
		}
		if tc.h == 0 {
			if l.Machines != tc.machines || l.Sessions != tc.sessions {
				t.Fatalf("unlimited: %+v", l)
			}
			continue
		}
		used := fixedLines + l.Graph + l.Machines + l.Sessions
		if l.Storage > 0 {
			used += l.Storage + storageFixed
		}
		if tc.h >= fixedLines+3 && used != tc.h && l.Graph < maxGraph {
			t.Errorf("%+v: layout %+v uses %d of %d lines", tc, l, used, tc.h)
		}
		if used > max(tc.h, fixedLines+3) {
			t.Errorf("%+v: layout %+v overflows (%d)", tc, l, used)
		}
		if l.Machines > tc.machines {
			t.Errorf("%+v: more machine rows than machines: %+v", tc, l)
		}
	}
	// A taller terminal gets taller graphs and more session rows.
	a, b := ComputeLayout(30, 3, 0, 100), ComputeLayout(60, 3, 0, 100)
	if b.Graph <= a.Graph || b.Sessions <= a.Sessions {
		t.Fatalf("30 lines %+v, 60 lines %+v", a, b)
	}
	// The rendered frame has exactly the terminal height.
	s := fakeSnapshot(100, now)
	for _, height := range []int{24, 40, 60} {
		v := DefaultView()
		v.Width, v.Height = 140, height
		if n := strings.Count(Frame(s, NewHistory(0), v, nil, PlainStyles()), "\n") + 1; n != height {
			t.Errorf("height %d: frame has %d lines", height, n)
		}
	}
}

func TestMachineSortByState(t *testing.T) {
	mk := func(id string, st provider.State, sessions int, idle time.Duration, age time.Duration) orchestrator.Machine {
		m := orchestrator.Machine{Machine: provider.Machine{ID: id, State: st, CreatedAt: now.Add(-age)}}
		if st == provider.Running {
			m.Status = &proto.Status{Ready: true}
			for i := 0; i < sessions; i++ {
				m.Status.Sessions = append(m.Status.Sessions, proto.Session{ID: id + fmt.Sprint(i)})
			}
			if sessions == 0 {
				m.Status.IdleSince = now.Add(-idle).Unix()
			}
		}
		return m
	}
	ms := []orchestrator.Machine{
		mk("failed", provider.Failed, 0, 0, time.Hour), mk("stopped", provider.Stopped, 0, 0, time.Hour),
		mk("idle-long", provider.Running, 0, 10*time.Minute, time.Hour), mk("starting", provider.Starting, 0, 0, time.Minute),
		mk("busy-young", provider.Running, 1, 0, time.Minute), mk("idle-short", provider.Running, 0, time.Minute, time.Hour),
		mk("busy-old", provider.Running, 2, 0, 2*time.Hour),
	}
	drain := mk("draining", provider.Running, 1, 0, time.Hour)
	drain.Status.Draining = true
	ms = append(ms, drain)
	SortMachines(ms, MSortState, false, now)
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	want := "busy-old busy-young idle-short idle-long starting draining stopped failed"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
	SortMachines(ms, MSortName, true, now)
	if ms[0].ID != "busy-old" || ms[len(ms)-1].ID != "stopped" {
		t.Fatalf("name sort %v", ms)
	}
}

// The session table lists running sessions, then queued requests, then
// recently finished sessions with exit code and duration.
func TestSessionRowsRunningQueuedFinished(t *testing.T) {
	homeDir = "/home/u"
	s := fakeSnapshot(100, now)
	h := NewHistory(0)
	h.Events([]events.Event{
		{Type: events.TaskPlaced, Task: "f1", Placement: "assign", Reason: "warm slot", WaitMs: 1500, Machine: "m-aws-1"},
		{Type: events.TaskEnd, Time: now.Add(-time.Minute), Task: "f1", Machine: "m-aws-1", Key: "r/cargo/clippy",
			Reserve: &scheduler.Resources{CPU: 4, Memory: 8 * gib}, Labels: map[string]string{"user": "u", "host": "box", "worktree": "/home/u/w"},
			Usage: &scheduler.Usage{Wall: 2 * time.Minute, ExitCode: 101, CPUSeconds: 240, PeakMemory: 2 * gib}},
	})
	rows := sessionRows(s, h, DefaultView(), nil)
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "run,run,queued,done" {
		t.Fatalf("kinds %v", kinds)
	}
	v := DefaultView()
	v.Width, v.Height = 160, 40
	out := Frame(s, h, v, nil, PlainStyles())
	for _, want := range []string{"exit 101", "cargo clippy", "2m", "2.0/4.0", "Sessions (2 running, 1 queued)"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q\n%s", want, out)
		}
	}
}

func storageSnapshot() Snapshot {
	s := fakeSnapshot(100, now)
	st0 := s.Machines[0].Status
	at := now.Add(-3 * time.Minute).Unix()
	st0.Disk = &proto.DiskStatus{Total: 200 * gib, Used: 120 * gib, Budget: 180 * gib, Items: []proto.StorageItem{
		{Kind: proto.StorageSlot, Name: "/cache/build/repo-app/0", Repo: "repo-app", Worktree: "/home/u/wt/feature", Branch: "wp/feature",
			Bytes: 30 * gib, LastUsed: now.Add(-time.Minute).Unix(), Committed: now.Add(-2 * time.Minute).Unix(), StoreAt: at, Stored: 28 * gib, Leased: true},
		{Kind: proto.StoragePages, Name: "repo-app", Repo: "repo-app", Bytes: 20 * gib, Resident: 20 * gib, Dirty: gib, Stored: 28 * gib, StoreAt: at,
			WorkingSet: 9 * gib, Classes: map[string]int64{"immutable": 10 * gib, "churn": 5 * gib}, TopDirs: []proto.DirSize{{Path: "target/debug/deps", Bytes: 12 * gib}}},
		{Kind: proto.StorageFiles, Name: "materialized files", Bytes: 8 * gib, Resident: 8 * gib},
		{Kind: proto.StorageMachineCache, Name: "usr__local__cargo__registry", Bytes: 2 * gib},
		{Kind: proto.StorageGlobal, Name: "sccache", Bytes: 3 * gib},
		{Kind: proto.StorageOther, Name: "images, system and unaccounted", Bytes: 5 * gib},
	}}
	st0.VM = &proto.VMStats{PageInBytes: 1 << 30, WritebackBytes: 1 << 29}
	return s
}

func TestStoragePanel(t *testing.T) {
	homeDir = "/home/u"
	s := storageSnapshot()
	h := NewHistory(0)
	h.Add(s)
	s2 := storageSnapshot()
	s2.Time = now.Add(10 * time.Second)
	vm2 := *s.Machines[0].Status.VM
	vm2.PageInBytes += 100 << 20
	s2.Machines[0].Status.VM = &vm2
	h.Add(s2)
	v := DefaultView()
	v.Width, v.Height = 170, 60
	out := Frame(s2, h, v, nil, PlainStyles())
	for _, want := range []string{"Storage  store 28.0G in 1 repos", "page-in 10M/s", "KIND", "NAME", "RESIDENT", "STORED", "LAST USED", "COMMITTED", "LAG",
		"budget 180.0G", "120.0G/200.0G", "~/wt/feature (wp/feature) *", "pages of repo-app", "materialized files", "usr__local__cargo__registry",
		"sccache", "machine-cache", "global", "images, system", "1m10s ago", "28.0G"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q\n%s", want, out)
		}
	}
	// Slot first, its page tier next, then the machine-wide items.
	if strings.Index(out, "(wp/feature)") > strings.Index(out, "pages of repo-app") || strings.Index(out, "pages of repo-app") > strings.Index(out, "materialized files") {
		t.Errorf("storage rows not grouped:\n%s", out)
	}
}

// Enter on a storage row opens its detail: access classes, largest
// directories, working set and storage events of the repository.
func TestStorageDetail(t *testing.T) {
	s := storageSnapshot()
	m := &model{snap: s, have: true, hist: NewHistory(0), view: DefaultView(), styles: PlainStyles()}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 50})
	m.hist.Events([]events.Event{
		{Type: events.SnapshotSave, Time: now.Add(-2 * time.Minute), Machine: "m-aws-1", Repo: "repo-app", Bytes: 2 * gib, Seconds: 1.5},
		{Type: events.SlotAction, Time: now.Add(-time.Minute), Machine: "m-aws-1", Repo: "repo-app", Reason: "prune", Bytes: gib},
		{Type: events.SlotAction, Time: now.Add(-time.Minute), Machine: "m-aws-1", Repo: "other", Reason: "prune", Bytes: gib},
	})
	m.key("tab")
	m.key("tab")
	if m.view.Focus != FocusStorage {
		t.Fatalf("focus %v", m.view.Focus)
	}
	m.key("down") // the slot row (after the machine header)
	m.key("enter")
	if !strings.HasPrefix(m.view.Detail, "storage:m-aws-1/slot/") {
		t.Fatalf("detail %q", m.view.Detail)
	}
	out := m.View()
	for _, want := range []string{"last session", "wp/feature", "store lag", "working set", "9.0G", "immutable", "churn", "Largest directories", "target/debug/deps",
		"snapshot_save", "slot_action", "prune"} {
		if !strings.Contains(out, want) {
			t.Errorf("storage detail lacks %q\n%s", want, out)
		}
	}
	if strings.Count(out, "slot_action") != 1 {
		t.Errorf("events of other repositories shown:\n%s", out)
	}
	m.key("esc")
	m.key("tab")
	if m.view.Focus != FocusSessions {
		t.Fatalf("tab cycles back to sessions: %v", m.view.Focus)
	}
}

// A machine up at the provider whose worker is not ready shows as
// booting with the time since its start, without capacity bars.
func TestBootingMachine(t *testing.T) {
	s := fakeSnapshot(100, now)
	s.Machines[1].State = provider.Running // up at the provider, no status yet
	s.Machines[1].Uptime = []orchestrator.Span{{Start: now.Add(-95 * time.Second)}}
	v := DefaultView()
	v.Width = 160
	out := Frame(s, NewHistory(0), v, nil, PlainStyles())
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "m-aws-2") {
			line = l
		}
	}
	if !strings.Contains(line, "booting 1m35s") || strings.Contains(line, "idle") || strings.Contains(line, "/16") {
		t.Fatalf("booting row %q", line)
	}
	s.Machines[1].Status = &proto.Status{Ready: false}
	if !strings.Contains(Frame(s, NewHistory(0), v, nil, PlainStyles()), "booting") {
		t.Fatal("not ready worker must show booting")
	}
	// Ready earlier in this uptime period, no status now: unreachable
	// since then, not booting since the start.
	s.Machines[1].Status, s.Machines[1].LastReady = nil, now.Add(-40*time.Second)
	if f := Frame(s, NewHistory(0), v, nil, PlainStyles()); !strings.Contains(f, "unreachable 40s") || strings.Contains(f, "booting") {
		t.Fatalf("lost worker must show unreachable:\n%s", f)
	}
	s.Machines[1].LastReady = time.Time{}
	s.Machines[1].Status = &proto.Status{Ready: true, Draining: true, DrainReason: "disk full"}
	if !strings.Contains(Frame(s, NewHistory(0), v, nil, PlainStyles()), "draining (disk full)") {
		t.Fatal("drain reason")
	}
}

func TestDetailViewsAndNavigation(t *testing.T) {
	homeDir = "/home/u"
	s := fakeSnapshot(108, now)
	m := &model{snap: s, have: true, hist: NewHistory(0), view: DefaultView(), styles: PlainStyles(), cur: map[string]float64{"s1": 4, "s2": 0.1}}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 45})
	m.hist.Add(s)
	m.hist.AddSessions(s, m.cur)
	m.hist.Events([]events.Event{{Type: events.TaskPlaced, Task: "s1", Placement: "assign", Reason: "warm slot", WaitMs: 2000},
		{Type: events.MachineState, Machine: "m-aws-1", State: "running", Time: now.Add(-time.Hour)}})
	// Session detail of the selected (longest running) session.
	m.key("enter")
	if m.view.Detail != "session:s2" {
		t.Fatalf("detail %q", m.view.Detail)
	}
	m.key("down")
	if m.view.Detail != "session:s1" {
		t.Fatalf("detail after down %q", m.view.Detail)
	}
	out := m.View()
	for _, want := range []string{"Session s1", "cargo build", "u@box", "~/cs-wt/feature-a", "wp/a", "assign: warm slot (waited 2s", "m-aws-1 (aws/c7a.8xlarge", "CPU 4.0 cores"} {
		if !strings.Contains(out, want) {
			t.Errorf("session detail lacks %q\n%s", want, out)
		}
	}
	m.key("esc")
	if m.view.Detail != "" {
		t.Fatal("esc must close the detail")
	}
	// Tab to machines, open the first (busy) machine.
	m.key("tab")
	m.key("enter")
	if m.view.Detail != "machine:m-aws-1" {
		t.Fatalf("machine detail %q", m.view.Detail)
	}
	out = m.View()
	for _, want := range []string{"Machine m-aws-1", "aws/c7a.8xlarge", "price", "$1.500/h", "cost today", "history", "running", "CPU busy", "Pressure", "Sessions", "cargo build"} {
		if !strings.Contains(out, want) {
			t.Errorf("machine detail lacks %q\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n") + 1; n > 45 {
		t.Errorf("machine detail has %d lines for 45", n)
	}
	m.key("q")
	if m.view.Detail != "" {
		t.Fatal("q closes the detail first")
	}
	if m.key("q") == nil {
		t.Fatal("q quits from the table view")
	}
}

func TestMouseSelectsAndOpens(t *testing.T) {
	s := fakeSnapshot(108, now)
	m := &model{snap: s, have: true, hist: NewHistory(0), view: DefaultView(), styles: PlainStyles()}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	frame := strings.Split(m.View(), "\n")
	row := func(text string) int {
		for i, l := range frame {
			if strings.Contains(l, text) {
				return i
			}
		}
		t.Fatalf("no line with %q", text)
		return -1
	}
	click := func(y int) {
		m.Update(tea.MouseMsg{X: 5, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	}
	click(row("m-aws-2"))
	if m.view.Focus != FocusMachines || Machines(m.snap, m.view)[m.view.MSelected].ID != "m-aws-2" {
		t.Fatalf("click on m-aws-2: %+v", m.view)
	}
	click(row("m-aws-2"))
	if m.view.Detail != "machine:m-aws-2" {
		t.Fatalf("second click opens: %q", m.view.Detail)
	}
	m.key("esc")
	click(row("cargo build"))
	if m.view.Focus != FocusSessions || sessionRows(m.snap, m.hist, m.view, m.cur)[m.view.Selected].Row.Session != "s1" {
		t.Fatalf("click on s1: %+v", m.view)
	}
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
	m.key("w")
	SortRows(rows, m.view.Sort, m.view.Asc)
	if m.view.Sort != SortWorktree || !m.view.Asc || rows[0].Session != "s1" {
		t.Fatalf("worktree sort %+v %+v", m.view, rows)
	}
	m.key("down")
	m.key("down")
	m.key("down")
	m.key("down") // clamped to the last row (2 running + 1 queued)
	if m.view.Selected != 2 {
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
	if !strings.Contains(m.View(), "WORKTREE▲") {
		t.Fatalf("sort marker missing:\n%s", m.View())
	}
	// Machine sort keys act on the machine table after Tab.
	m.key("tab")
	m.key("n")
	if m.view.MSort != MSortName || m.view.Sort != SortWorktree {
		t.Fatalf("machine sort %+v", m.view)
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
	v.Width = 200
	out := Frame(s, NewHistory(0), v, nil, PlainStyles())
	for _, want := range []string{"CACHE", "DIRTY", "WS", "FAULTS", "40.0G/50.0G", "512M", "60.0G", "12.5/s", "THRASH"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Narrow frames drop the cache columns before the rest.
	v.Width = 100
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
