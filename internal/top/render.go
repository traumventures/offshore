package top

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// SortKey selects the session table order.
type SortKey int

// Session sort keys (keyboard: e c m w n u).
const (
	SortElapsed SortKey = iota
	SortCPU
	SortMem
	SortWorktree
	SortMachine
	SortUser
)

var sortNames = map[SortKey]string{SortElapsed: "ELAPSED", SortCPU: "CPU", SortMem: "MEM", SortWorktree: "WORKTREE", SortMachine: "MACHINE", SortUser: "USER@HOST"}

// MSortKey selects the machine table order.
type MSortKey int

// Machine sort keys (keyboard with the machine table focused: s c m d p a n).
const (
	MSortState MSortKey = iota // running busy, idle (by idle time), starting, draining, stopped, failed; then age
	MSortCPU
	MSortMem
	MSortDisk
	MSortPrice
	MSortAge
	MSortName
)

var msortNames = map[MSortKey]string{MSortState: "STATE", MSortCPU: "CPU", MSortMem: "MEM", MSortDisk: "DISK", MSortPrice: "$/H", MSortAge: "AGE", MSortName: "MACHINE"}

// Focus is the table that keys and the selection act on.
type Focus int

const (
	FocusSessions Focus = iota
	FocusMachines
)

// Windows are the zoom levels of the history graphs (+/-).
var Windows = []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

// View is the presentation state of a frame.
type View struct {
	Window time.Duration
	Sort   SortKey
	Asc    bool // default descending
	MSort  MSortKey
	MAsc   bool
	Focus  Focus
	// Selected and Offset are the selected and first visible session
	// table rows; MSelected and MOffset those of the machine table.
	Selected, Offset   int
	MSelected, MOffset int
	Width              int
	Height             int // 0: unlimited (print every row)
	// Detail is the open detail view ("" none, "machine:<id>",
	// "session:<id>").
	Detail string
}

// DefaultView is the initial view.
func DefaultView() View { return View{Window: 10 * time.Minute, Sort: SortElapsed, Width: 120} }

// Row is one running session.
type Row struct {
	Machine, Session        string
	Agent, Worktree, Branch string
	Client                  string // user@host
	Command                 string
	Labels                  map[string]string
	Elapsed                 time.Duration
	ResCPU, BusyCPU         float64
	ResMem, UsedMem         int64
}

// Rows lists the sessions of a snapshot. rates holds busy cores per
// session measured between polls; sessions without a rate use their mean
// (CPU seconds / elapsed).
func Rows(s Snapshot, rates map[string]float64) []Row {
	var out []Row
	for _, m := range s.Up() {
		if m.Status == nil {
			continue
		}
		for _, se := range m.Status.Sessions {
			el := s.Time.Sub(time.Unix(se.Started, 0))
			if el < 0 {
				el = 0
			}
			busy, ok := rates[se.ID]
			if !ok && el > 0 {
				busy = se.CPUSeconds / el.Seconds()
			}
			wt := se.Labels[labels.Worktree]
			if wt == "" {
				wt = se.Repo
			}
			out = append(out, Row{
				Machine: m.ID, Session: se.ID, Agent: se.Labels[labels.Agent], Worktree: wt, Branch: se.Labels[labels.Branch],
				Client: labels.UserHost(se.Labels), Command: strings.Join(se.Argv, " "), Labels: se.Labels, Elapsed: el,
				ResCPU: se.CPU, BusyCPU: busy, ResMem: se.Memory, UsedMem: se.UsedMemory,
			})
		}
	}
	return out
}

// SortRows orders rows by key (descending unless asc), ties by session.
func SortRows(rows []Row, key SortKey, asc bool) {
	less := func(a, b Row) int {
		switch key {
		case SortCPU:
			return cmpF(a.BusyCPU, b.BusyCPU)
		case SortMem:
			return cmpF(float64(a.UsedMem), float64(b.UsedMem))
		case SortWorktree:
			return strings.Compare(a.Worktree, b.Worktree)
		case SortMachine:
			return strings.Compare(a.Machine, b.Machine)
		case SortUser:
			return strings.Compare(a.Client, b.Client)
		}
		return cmpF(float64(a.Elapsed), float64(b.Elapsed))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		c := less(rows[i], rows[j])
		if c == 0 {
			return rows[i].Session < rows[j].Session
		}
		if asc {
			return c < 0
		}
		return c > 0
	})
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// stateRank orders machines for MSortState: running busy, running idle,
// provisioning/starting, draining, stopped, failed.
func stateRank(m orchestrator.Machine) int {
	st := m.Status
	switch {
	case m.State == provider.Running && st != nil && st.Ready && (st.Draining || m.Draining):
		return 3
	case m.State == provider.Running && st != nil && st.Ready && len(st.Sessions) > 0:
		return 0
	case m.State == provider.Running && st != nil && st.Ready:
		return 1
	case m.State == provider.Running || m.State == provider.Starting:
		return 2
	case m.State == provider.Stopped:
		return 4
	}
	return 5
}

// SortMachines orders machines by key (MSortState: state rank, idle
// machines by idle time, then age; others descending unless asc).
func SortMachines(ms []orchestrator.Machine, key MSortKey, asc bool, now time.Time) {
	idle := func(m orchestrator.Machine) time.Duration {
		if m.Status != nil && m.Status.IdleSince > 0 {
			return now.Sub(time.Unix(m.Status.IdleSince, 0))
		}
		return 0
	}
	disk := func(m orchestrator.Machine) float64 {
		if m.Status != nil && m.Status.Disk != nil {
			return float64(m.Status.Disk.Used)
		}
		return 0
	}
	cmp := func(a, b orchestrator.Machine) int {
		switch key {
		case MSortCPU:
			return cmpF(measured(a).CPU, measured(b).CPU)
		case MSortMem:
			return cmpF(float64(measured(a).Memory), float64(measured(b).Memory))
		case MSortDisk:
			return cmpF(disk(a), disk(b))
		case MSortPrice:
			return cmpF(a.PricePerHour, b.PricePerHour)
		case MSortAge:
			return cmpF(float64(now.Sub(a.CreatedAt)), float64(now.Sub(b.CreatedAt)))
		case MSortName:
			return strings.Compare(a.ID, b.ID)
		}
		return 0
	}
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if key == MSortState {
			if ra, rb := stateRank(a), stateRank(b); ra != rb {
				return (ra < rb) != asc
			}
			if ia, ib := idle(a), idle(b); ia != ib {
				return ia < ib
			}
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID < b.ID
		}
		c := cmp(a, b)
		if c == 0 {
			return a.ID < b.ID
		}
		if asc {
			return c < 0
		}
		return c > 0
	})
}

func measured(m orchestrator.Machine) scheduler.Resources {
	if m.Status != nil {
		return m.Status.Measured
	}
	return scheduler.Resources{}
}

// Machines returns the machines of a snapshot in the view's order.
func Machines(s Snapshot, v View) []orchestrator.Machine {
	ms := s.Up()
	SortMachines(ms, v.MSort, v.MAsc, s.Time)
	return ms
}

// Styles are the frame's styles; a renderer without colors gives plain
// text (only box drawing and block characters).
type Styles struct {
	Panel, Title, Header, Selected, Dim, Warn, Good, Bar, BarBg, Spark lipgloss.Style
}

// NewStyles builds styles for a renderer.
func NewStyles(r *lipgloss.Renderer) Styles {
	return Styles{
		Panel:    r.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")),
		Title:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("14")),
		Header:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")),
		Selected: r.NewStyle().Reverse(true),
		Dim:      r.NewStyle().Foreground(lipgloss.Color("8")),
		Warn:     r.NewStyle().Foreground(lipgloss.Color("11")),
		Good:     r.NewStyle().Foreground(lipgloss.Color("10")),
		Bar:      r.NewStyle().Foreground(lipgloss.Color("10")),
		BarBg:    r.NewStyle().Foreground(lipgloss.Color("8")),
		Spark:    r.NewStyle().Foreground(lipgloss.Color("12")),
	}
}

// Layout is the vertical split of a frame.
type Layout struct {
	Graph    int // sparkline rows of the graph panels
	Machines int // visible machine rows
	Storage  int // storage panel rows (0: hidden)
	Sessions int // visible session table rows
}

// Fixed lines: header and help (2), graph panels (border and title: 3),
// machines and sessions panels (border, title, column header: 4 each),
// storage panel (border, title, column header: 4).
const (
	fixedLines   = 2 + 3 + 4 + 4
	storageFixed = 4
	maxGraph     = 10
)

// ComputeLayout splits height lines between the graphs, the machine
// table, the storage panel and the session table. height 0 is unlimited
// (every row, graphs of 3 rows). Tiny terminals keep one row each.
func ComputeLayout(height, machines, storage, sessions int) Layout {
	if height <= 0 {
		return Layout{Graph: 3, Machines: max(machines, 1), Storage: storage, Sessions: max(sessions, 1)}
	}
	avail := height - fixedLines
	if avail < 3 {
		return Layout{Graph: 1, Machines: 1, Sessions: 1}
	}
	l := Layout{Graph: min(max(avail/4, 1), maxGraph)}
	rest := avail - l.Graph
	if storage > 0 && rest >= 10 {
		l.Storage = min(storage, max((rest-storageFixed)/5, 1))
		rest -= l.Storage + storageFixed
	}
	l.Machines = min(max(machines, 1), max(rest/2, 1))
	l.Sessions = max(rest-l.Machines, 1)
	// Fewer sessions than room: give the rest to the graphs.
	if sessions > 0 && l.Sessions > sessions+2 {
		spare := l.Sessions - (sessions + 2)
		g := min(spare, maxGraph-l.Graph)
		l.Graph += g
		l.Sessions -= g
	}
	return l
}

// Frame renders one screen.
func Frame(s Snapshot, h *History, v View, rates map[string]float64, st Styles) string {
	v = normalize(v)
	if h == nil {
		h = NewHistory(0)
	}
	if v.Detail != "" {
		return detail(s, h, v, rates, st)
	}
	ms := Machines(s, v)
	srows := sessionRows(s, h, v, rates)
	stor := storageRows(s)
	l := ComputeLayout(v.Height, len(ms), len(stor), len(srows))
	var b strings.Builder
	b.WriteString(header(s, v, st) + "\n")
	b.WriteString(graphs(s, h, v, st, l.Graph))
	b.WriteString(machinesPanel(s, ms, v, st, l.Machines))
	if l.Storage > 0 {
		b.WriteString(storagePanel(s, h, stor, v, st, l.Storage))
	}
	b.WriteString(sessionsPanel(srows, v, st, l.Sessions))
	b.WriteString(st.Dim.Render(fit(helpLine(v), v.Width)))
	return b.String()
}

func normalize(v View) View {
	if v.Width < 60 {
		v.Width = 60
	}
	if v.Window <= 0 {
		v.Window = 10 * time.Minute
	}
	return v
}

func helpLine(v View) string {
	if v.Focus == FocusMachines {
		return "↑/↓ select  Tab sessions  Enter detail  s/c/m/d/p/a/n sort  r reverse  +/- window  q quit"
	}
	return "↑/↓ select  Tab machines  Enter detail  e/c/m/w/n/u sort  r reverse  +/- window  q quit"
}

func header(s Snapshot, v View, st Styles) string {
	up, run := 0, 0
	sessions := 0
	for _, m := range s.Up() {
		up++
		if running(m) {
			run++
		}
		if m.Status != nil {
			sessions += len(m.Status.Sessions)
		}
	}
	q := fmt.Sprint(len(s.Queue))
	if !s.QueueKnown {
		q = "?"
	}
	left := st.Title.Render("offshore top") + "  " + s.Time.Local().Format("15:04:05")
	right := fmt.Sprintf("machines %d (%d up)  sessions %d  queue %s  window %s", up, run, sessions, q, short(v.Window))
	gap := v.Width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return fit(left+"  "+right, v.Width)
	}
	return left + strings.Repeat(" ", gap) + right
}

// graphs draws the CPU and memory history of the active machines side by
// side, rows high.
func graphs(s Snapshot, h *History, v View, st Styles, rows int) string {
	w := v.Width / 2
	inner := w - 2
	last := Point{}
	if n := len(h.Pool); n > 0 {
		last = h.Pool[n-1]
	}
	at := func(p Point) time.Time { return p.T }
	maxOf := func(f func(Point) float64) float64 {
		m := 0.0
		for _, p := range h.Pool {
			if p.T.After(s.Time.Add(-v.Window)) {
				m = math.Max(m, f(p))
			}
		}
		return m
	}
	type g struct {
		title string
		val   func(Point) float64
		top   float64
	}
	gs := []g{
		{fmt.Sprintf("CPU busy %.1f / %.0f cores (reserved %.1f)", last.BusyCPU, last.CapCPU, last.ReservedCPU), func(p Point) float64 { return p.BusyCPU },
			maxOf(func(p Point) float64 { return p.CapCPU })},
		{fmt.Sprintf("Memory %s / %s", human(last.UsedMem), human(last.CapMem)), func(p Point) float64 { return float64(p.UsedMem) },
			maxOf(func(p Point) float64 { return float64(p.CapMem) })},
	}
	var panels []string
	for i, x := range gs {
		pw := inner
		if i == 1 {
			pw = v.Width - w - 2
		}
		vals, ok := Series(h.Pool, at, x.val, s.Time, v.Window, pw)
		body := st.Spark.Render(Sparkline(vals, ok, x.top, rows))
		panels = append(panels, st.Panel.Width(pw).Render(st.Title.Render(fit(x.title, pw))+"\n"+body))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, panels...) + "\n"
}

// panel draws a titled panel of lines, padded to n lines when n > 0.
func panel(title string, lines []string, n, inner int, st Styles) string {
	for n > 0 && len(lines) < n {
		lines = append(lines, "")
	}
	return st.Panel.Width(inner).Render(title+"\n"+strings.Join(lines, "\n")) + "\n"
}

// window returns the first visible row so that sel is visible among n
// rows of total.
func window(sel, off, n, total int) int {
	if n <= 0 || total <= n {
		return 0
	}
	if sel < off {
		off = sel
	}
	if sel >= off+n {
		off = sel - n + 1
	}
	return min(max(off, 0), total-n)
}

func sortMark(asc bool) string {
	if asc {
		return "▲"
	}
	return "▼"
}

var machineCols = []Col{
	{Title: "MACHINE"}, {Title: "PROVIDER/SIZE"}, {Title: "STATE"},
	{Title: "CPU", Bar: true}, {Title: "MEM", Bar: true}, {Title: "DISK", Bar: true, Drop: 1},
	{Title: "SESS", Right: true}, {Title: "$/H", Right: true},
	{Title: "CACHE", Right: true, Drop: 2}, {Title: "DIRTY", Right: true, Drop: 3}, {Title: "WS", Right: true, Drop: 4}, {Title: "FAULTS", Right: true, Drop: 5},
}

// machineState is the displayed state of a machine.
func machineState(m orchestrator.Machine, now time.Time) string {
	state := string(m.State)
	if st0 := m.Status; st0 != nil && m.State == provider.Running {
		switch {
		case !st0.Ready:
			state = "provisioning"
		case st0.Draining || m.Draining:
			state = "draining"
		case len(st0.Sessions) == 0 && st0.IdleSince > 0:
			state = "idle " + short(now.Sub(time.Unix(st0.IdleSince, 0)))
		case len(st0.Sessions) > 0:
			state = "busy"
		}
	}
	return state
}

func machineRow(m orchestrator.Machine, now time.Time, st Styles) []Cell {
	size := m.Labels[provider.LabelSize]
	if size == "" {
		size = "-"
	}
	row := []Cell{C(m.ID), C(m.Provider + "/" + size), C(machineState(m, now)), C(""), C(""), C(""), C("-"), C(fmt.Sprintf("%.2f", m.PricePerHour)),
		C(""), C(""), C(""), C("")}
	active := m.State == provider.Running || m.State == provider.Starting
	if active {
		busy, mem := 0.0, int64(0)
		if st0 := m.Status; st0 != nil {
			busy, mem = st0.Measured.CPU, st0.Measured.Memory
			row[6] = C(fmt.Sprint(len(st0.Sessions)))
			if d := st0.Disk; d != nil && d.Total > 0 {
				row[5] = B(float64(d.Used), float64(d.Total), human(d.Used)+"/"+human(d.Total))
			}
			if vm := st0.VM; vm != nil {
				res := human(vm.ResidentBytes)
				if vm.Capacity > 0 {
					res += "/" + human(vm.Capacity)
				}
				row[8], row[9], row[10] = C(res), C(human(vm.DirtyBytes)), C(human(vm.WorkingSetBytes))
				f := fmt.Sprintf("%.1f/s", vm.FaultRate)
				if vm.Thrashing {
					f = st.Warn.Render("THRASH") + " " + f
				}
				row[11] = C(f)
			}
		}
		row[3] = B(busy, m.CPU, fmt.Sprintf("%.1f/%g", busy, m.CPU))
		row[4] = B(float64(mem), float64(m.Memory), human(mem)+"/"+human(m.Memory))
	}
	return row
}

func machinesPanel(s Snapshot, ms []orchestrator.Machine, v View, st Styles, n int) string {
	inner := v.Width - 2
	rows := make([][]Cell, len(ms))
	for i, m := range ms {
		rows[i] = machineRow(m, s.Time, st)
	}
	ws := layoutCols(machineCols, rows, inner)
	marks := map[int]string{}
	if i := map[MSortKey]int{MSortName: 0, MSortState: 2, MSortCPU: 3, MSortMem: 4, MSortDisk: 5, MSortPrice: 7}[v.MSort]; v.MSort != MSortAge {
		marks[i] = sortMark(v.MAsc)
	}
	lines := []string{st.Header.Render(fit(renderHeader(machineCols, ws, marks), inner))}
	limit := n
	if v.Height <= 0 {
		limit = len(rows)
	}
	sel := min(max(v.MSelected, 0), max(len(rows)-1, 0))
	off := window(sel, v.MOffset, limit, len(rows))
	for i := off; i < len(rows) && i < off+limit; i++ {
		line := fit(renderRow(machineCols, ws, rows[i], st), inner)
		if i == sel && v.Focus == FocusMachines && v.Height > 0 {
			line = st.Selected.Render(pad(line, inner))
		}
		lines = append(lines, line)
	}
	if len(rows) == 0 {
		lines = append(lines, st.Dim.Render("no machines"))
	}
	title := fmt.Sprintf("Machines (%d)  $%.2f/h now · $%.2f today", len(ms), s.Cost.PerHour, s.Cost.Total)
	if v.Focus == FocusMachines {
		title += "  sort " + msortNames[v.MSort]
	}
	pad := 0
	if v.Height > 0 {
		pad = limit + 1
	}
	return panel(st.Title.Render(fit(title, inner)), lines, pad, inner, st)
}

// SRow is a row of the session table: a running session, a queued
// request or a recently finished session.
type SRow struct {
	Kind     string // "run", "queued", "done"
	Row      Row
	Finished *Finished
}

// sessionRows lists running sessions (sorted), then queued requests, then
// recently finished sessions (newest first).
func sessionRows(s Snapshot, h *History, v View, rates map[string]float64) []SRow {
	rows := Rows(s, rates)
	SortRows(rows, v.Sort, v.Asc)
	var out []SRow
	for _, r := range rows {
		out = append(out, SRow{Kind: "run", Row: r})
	}
	for _, q := range s.Queue {
		wt := q.Labels[labels.Worktree]
		if wt == "" {
			wt = q.RepoID
		}
		out = append(out, SRow{Kind: "queued", Row: Row{Session: q.ID, Worktree: wt, Branch: q.Labels[labels.Branch], Client: labels.UserHost(q.Labels),
			Command: keyCommand(q.Key), Labels: q.Labels, Elapsed: s.Time.Sub(q.Since), ResCPU: q.Need.CPU, ResMem: q.Need.Memory}})
	}
	running := map[string]bool{}
	for _, r := range rows {
		running[r.Session] = true
	}
	for i := len(h.Finished) - 1; i >= 0; i-- {
		f := h.Finished[i]
		if running[f.ID] {
			continue
		}
		f2 := f
		wt := f.Labels[labels.Worktree]
		r := Row{Machine: f.Machine, Session: f.ID, Worktree: wt, Branch: f.Labels[labels.Branch], Client: labels.UserHost(f.Labels),
			Command: keyCommand(f.Key), Labels: f.Labels, Elapsed: f.Wall, ResCPU: f.Reserve.CPU, ResMem: f.Reserve.Memory, UsedMem: f.PeakMem}
		if f.Wall > 0 {
			r.BusyCPU = f.CPUSeconds / f.Wall.Seconds()
		}
		out = append(out, SRow{Kind: "done", Row: r, Finished: &f2})
	}
	return out
}

// keyCommand renders a learning key (repo/tool/subcommand) as a command.
func keyCommand(key string) string {
	if _, rest, ok := strings.Cut(key, "/"); ok {
		return strings.ReplaceAll(rest, "/", " ")
	}
	return key
}

var sessionCols = []Col{
	{Title: "MACHINE"}, {Title: "SESSION"}, {Title: "STATE"}, {Title: "USER@HOST", Drop: 3},
	{Title: "WORKTREE", Flex: true, Min: 12}, {Title: "BRANCH", Drop: 4}, {Title: "ELAPSED", Right: true},
	{Title: "CPU", Bar: true}, {Title: "MEM", Bar: true}, {Title: "COMMAND", Flex: true, Min: 10},
}

func sessionCells(r SRow) []Cell {
	x := r.Row
	state := "running"
	switch r.Kind {
	case "queued":
		state = "queued"
	case "done":
		state = fmt.Sprintf("exit %d", r.Finished.Exit)
		if r.Finished.OOM {
			state += " OOM"
		}
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	cpu := B(x.BusyCPU, x.ResCPU, fmt.Sprintf("%.1f/%.1f", x.BusyCPU, x.ResCPU))
	mem := B(float64(x.UsedMem), float64(x.ResMem), human(x.UsedMem)+"/"+human(x.ResMem))
	if r.Kind == "queued" {
		cpu, mem = C(fmt.Sprintf("need %.1f", x.ResCPU)), C("need "+human(x.ResMem))
	}
	return []Cell{C(dash(x.Machine)), C(trunc(x.Session, 8)), C(state), C(dash(x.Client)), C(shortPath(dash(x.Worktree))),
		C(dash(x.Branch)), C(short(x.Elapsed)), cpu, mem, C(x.Command)}
}

func sessionsPanel(rows []SRow, v View, st Styles, n int) string {
	inner := v.Width - 2
	cells := make([][]Cell, len(rows))
	live := 0
	for i, r := range rows {
		cells[i] = sessionCells(r)
		if r.Kind != "done" {
			live++
		}
	}
	ws := layoutCols(sessionCols, cells, inner)
	marks := map[int]string{}
	marks[map[SortKey]int{SortMachine: 0, SortUser: 3, SortWorktree: 4, SortElapsed: 6, SortCPU: 7, SortMem: 8}[v.Sort]] = sortMark(v.Asc)
	lines := []string{st.Header.Render(fit(renderHeader(sessionCols, ws, marks), inner))}
	limit := n
	if v.Height <= 0 {
		limit = live // --once: running and queued only
		if limit == 0 {
			limit = min(len(rows), 5)
		}
	}
	sel := min(max(v.Selected, 0), max(len(rows)-1, 0))
	off := window(sel, v.Offset, limit, len(rows))
	for i := off; i < len(rows) && i < off+limit; i++ {
		line := renderRow(sessionCols, ws, cells[i], st)
		if i < len(rows) && rows[i].Kind == "done" {
			line = st.Dim.Render(line)
		}
		line = fit(line, inner)
		if i == sel && v.Focus == FocusSessions && v.Height > 0 {
			line = st.Selected.Render(pad(line, inner))
		}
		lines = append(lines, line)
	}
	if len(rows) == 0 {
		lines = append(lines, st.Dim.Render("no sessions"))
	}
	title := fmt.Sprintf("Sessions (%d running, %d queued)", countKind(rows, "run"), countKind(rows, "queued"))
	if v.Focus == FocusSessions {
		title += "  sort " + sortNames[v.Sort]
	}
	pad := 0
	if v.Height > 0 {
		pad = limit + 1
	}
	return panel(st.Title.Render(fit(title, inner)), lines, pad, inner, st)
}

func countKind(rows []SRow, k string) int {
	n := 0
	for _, r := range rows {
		if r.Kind == k {
			n++
		}
	}
	return n
}

// storageRow is one build slot (machine, repo) or a repository head
// without a slot on any running machine.
type storageRow struct {
	Repo, Machine string
	Slot          int64 // slot bytes on the machine (0: none)
	Head          int64 // newest head bytes in the store
	HeadAt        time.Time
	Store         string
	MachineHead   time.Time // newest head committed from this machine
}

// storageRows lists the build slots of running machines and the store
// heads of their repositories.
func storageRows(s Snapshot) []storageRow {
	type head struct {
		bytes int64
		at    time.Time
		store string
	}
	heads := map[string]head{}
	mheads := map[[2]string]time.Time{}
	for _, m := range s.Up() {
		if m.Status == nil {
			continue
		}
		for _, hd := range m.Status.Heads {
			at := time.Unix(hd.At, 0)
			if cur, ok := heads[hd.Repo]; !ok || at.After(cur.at) {
				heads[hd.Repo] = head{hd.Bytes, at, hd.Store}
			}
			k := [2]string{m.ID, hd.Repo}
			if at.After(mheads[k]) {
				mheads[k] = at
			}
		}
	}
	var out []storageRow
	seen := map[string]bool{}
	for _, m := range s.Up() {
		if m.Status == nil || m.Status.Disk == nil {
			continue
		}
		for repo, b := range m.Status.Disk.SlotBytes {
			hd := heads[repo]
			out = append(out, storageRow{Repo: repo, Machine: m.ID, Slot: b, Head: hd.bytes, HeadAt: hd.at, Store: hd.store, MachineHead: mheads[[2]string{m.ID, repo}]})
			seen[repo] = true
		}
	}
	for repo, hd := range heads {
		if !seen[repo] {
			out = append(out, storageRow{Repo: repo, Head: hd.bytes, HeadAt: hd.at, Store: hd.store})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot > out[j].Slot
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Machine < out[j].Machine
	})
	return out
}

var storageCols = []Col{
	{Title: "REPO"}, {Title: "MACHINE"}, {Title: "SLOT", Right: true}, {Title: "HEAD", Right: true},
	{Title: "COMMITTED", Right: true}, {Title: "LAG", Right: true, Drop: 2}, {Title: "STORE", Drop: 1},
}

func storageCells(r storageRow, now time.Time) []Cell {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	slot, head, committed, lag := "-", "-", "-", "-"
	if r.Slot > 0 {
		slot = human(r.Slot)
	}
	if r.Head > 0 {
		head = human(r.Head)
	}
	if !r.HeadAt.IsZero() {
		committed = short(now.Sub(r.HeadAt)) + " ago"
	}
	if r.Machine != "" && !r.HeadAt.IsZero() {
		if r.MachineHead.IsZero() {
			lag = "not committed"
		} else {
			lag = short(r.HeadAt.Sub(r.MachineHead))
		}
	}
	return []Cell{C(trunc(r.Repo, 20)), C(dash(r.Machine)), C(slot), C(head), C(committed), C(lag), C(dash(r.Store))}
}

func storagePanel(s Snapshot, h *History, rows []storageRow, v View, st Styles, n int) string {
	inner := v.Width - 2
	cells := make([][]Cell, len(rows))
	stored := map[string]int64{}
	for i, r := range rows {
		cells[i] = storageCells(r, s.Time)
		if r.Head > stored[r.Repo] {
			stored[r.Repo] = r.Head
		}
	}
	var total int64
	for _, b := range stored {
		total += b
	}
	var pin, wb float64
	for _, m := range s.Up() {
		if ps := h.Machine[m.ID]; running(m) && len(ps) > 0 {
			pin += ps[len(ps)-1].PageIn
			wb += ps[len(ps)-1].Writeback
		}
	}
	ws := layoutCols(storageCols, cells, inner)
	lines := []string{st.Header.Render(fit(renderHeader(storageCols, ws, nil), inner))}
	limit := n
	if v.Height <= 0 {
		limit = len(rows)
	}
	for i := 0; i < len(rows) && i < limit; i++ {
		lines = append(lines, fit(renderRow(storageCols, ws, cells[i], st), inner))
	}
	title := fmt.Sprintf("Storage  store %s in %d repos  page-in %s/s  writeback %s/s", human(total), len(stored), human(int64(pin)), human(int64(wb)))
	if len(rows) > limit {
		title += fmt.Sprintf("  (%d more)", len(rows)-limit)
	}
	pad := 0
	if v.Height > 0 {
		pad = limit + 1
	}
	return panel(st.Title.Render(fit(title, inner)), lines, pad, inner, st)
}

var blocks = []rune(" ▁▂▃▄▅▆▇█")

// Sparkline draws vals scaled to top over height rows (eighth-block
// resolution); columns without data are blank.
func Sparkline(vals []float64, ok []bool, top float64, height int) string {
	if height < 1 {
		height = 1
	}
	if top <= 0 {
		for _, x := range vals {
			top = math.Max(top, x)
		}
	}
	rows := make([][]rune, height)
	for r := range rows {
		rows[r] = make([]rune, len(vals))
		for c := range rows[r] {
			rows[r][c] = ' '
		}
	}
	for c, x := range vals {
		if !ok[c] {
			continue
		}
		level := 0
		if top > 0 {
			level = int(math.Round(math.Min(1, math.Max(0, x/top)) * float64(height*8)))
		}
		for r := 0; r < height; r++ { // r = 0 is the bottom row
			n := min(8, max(0, level-r*8))
			if r == 0 {
				n = max(n, 1) // baseline: the column has data
			}
			rows[height-1-r][c] = blocks[n]
		}
	}
	out := make([]string, height)
	for i, r := range rows {
		out[i] = string(r)
	}
	return strings.Join(out, "\n")
}

func human(b int64) string {
	const g = 1 << 30
	switch {
	case b <= 0:
		return "0"
	case b >= g:
		return fmt.Sprintf("%.1fG", float64(b)/g)
	}
	return fmt.Sprintf("%dM", b>>20)
}

// short renders a duration compactly: 45s, 12m, 3h05m.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// shortPath abbreviates the home directory.
func shortPath(p string) string {
	if home := homeDir; home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// homeDir is abbreviated as ~ (replaced in tests).
var homeDir, _ = os.UserHomeDir()

func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// truncLeft keeps the end of s (paths).
func truncLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w || w <= 1 {
		return trunc(s, w)
	}
	r := []rune(s)
	for len(r) > 0 && ansi.StringWidth(string(r)) > w-1 {
		r = r[1:]
	}
	return "…" + string(r)
}

func pad(s string, w int) string {
	s = trunc(s, w)
	if n := w - ansi.StringWidth(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// fit truncates (styled) s to w cells.
func fit(s string, w int) string { return ansi.Truncate(s, w, "…") }

// PlainStyles are styles without colors or attributes (for --once).
func PlainStyles() Styles {
	return NewStyles(lipgloss.NewRenderer(io.Discard, termenv.WithProfile(termenv.Ascii)))
}
