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

	"github.com/traumventures/cloudsourced/internal/labels"
	"github.com/traumventures/cloudsourced/internal/provider"
)

// SortKey selects the session table order.
type SortKey int

// Sort keys (keyboard: e c m a w n).
const (
	SortElapsed SortKey = iota
	SortCPU
	SortMem
	SortAgent
	SortWorktree
	SortMachine
)

var sortNames = map[SortKey]string{SortElapsed: "ELAPSED", SortCPU: "CPU", SortMem: "MEM", SortAgent: "AGENT", SortWorktree: "WORKTREE", SortMachine: "MACHINE"}

// Windows are the zoom levels of the history graphs (+/-).
var Windows = []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

// View is the presentation state of a frame.
type View struct {
	Window   time.Duration
	Sort     SortKey
	Asc      bool // default descending
	Selected int  // index into the sorted session rows
	Offset   int  // first visible row
	Width    int
	Height   int // 0: unlimited (print every row)
}

// DefaultView is the initial view.
func DefaultView() View { return View{Window: 10 * time.Minute, Sort: SortElapsed, Width: 120} }

// Row is one running session.
type Row struct {
	Machine, Session        string
	Agent, Worktree, Branch string
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
				Command: strings.Join(se.Argv, " "), Labels: se.Labels, Elapsed: el,
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
		case SortAgent:
			return strings.Compare(a.Agent, b.Agent)
		case SortWorktree:
			return strings.Compare(a.Worktree, b.Worktree)
		case SortMachine:
			return strings.Compare(a.Machine, b.Machine)
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

// Frame renders one screen.
func Frame(s Snapshot, h *History, v View, rates map[string]float64, st Styles) string {
	if v.Width < 60 {
		v.Width = 60
	}
	if v.Window <= 0 {
		v.Window = 10 * time.Minute
	}
	if h == nil {
		h = NewHistory(0)
	}
	var b strings.Builder
	b.WriteString(header(s, v, st) + "\n")
	b.WriteString(graphs(s, h, v, st))
	b.WriteString(machines(s, h, v, st))
	used := strings.Count(b.String(), "\n")
	rows := Rows(s, rates)
	SortRows(rows, v.Sort, v.Asc)
	queueLines := 0
	if len(s.Queue) > 0 {
		queueLines = len(s.Queue) + 3
	}
	avail := 0 // unlimited
	if v.Height > 0 {
		avail = max(1, v.Height-used-queueLines-4) // table border, header, help
	}
	b.WriteString(table(rows, v, avail, st))
	if queueLines > 0 {
		b.WriteString(queue(s, v, st))
	}
	b.WriteString(st.Dim.Render(fit("↑/↓ select  e/c/m/a/w/n sort  r reverse  +/- window  q quit", v.Width)))
	return b.String()
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
	left := st.Title.Render("cloudsourced top") + "  " + s.Time.Local().Format("15:04:05")
	right := fmt.Sprintf("machines %d (%d up)  sessions %d  queue %s  $%.2f/h now  $%.2f today  window %s",
		up, run, sessions, q, s.Cost.PerHour, s.Cost.Total, short(v.Window))
	gap := v.Width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return fit(left+"  "+right, v.Width)
	}
	return left + strings.Repeat(" ", gap) + right
}

// graphs draws the four pool history panels in a 2×2 grid.
func graphs(s Snapshot, h *History, v View, st Styles) string {
	w := v.Width / 2
	inner := w - 2
	last := Point{}
	if n := len(h.Pool); n > 0 {
		last = h.Pool[n-1]
	}
	at := func(p Point) time.Time { return p.T }
	type g struct {
		title string
		val   func(Point) float64
		top   float64
	}
	maxOf := func(f func(Point) float64) float64 {
		m := 0.0
		for _, p := range h.Pool {
			if p.T.After(s.Time.Add(-v.Window)) {
				m = math.Max(m, f(p))
			}
		}
		return m
	}
	capCPU := maxOf(func(p Point) float64 { return p.CapCPU })
	capMem := maxOf(func(p Point) float64 { return float64(p.CapMem) })
	sessQ := maxOf(func(p Point) float64 { return float64(p.Sessions + p.Queue) })
	cost := maxOf(func(p Point) float64 { return p.CostPerHour })
	gs := []g{
		{fmt.Sprintf("CPU busy %.1f / %.0f cores (reserved %.1f)", last.BusyCPU, last.CapCPU, last.ReservedCPU), func(p Point) float64 { return p.BusyCPU }, capCPU},
		{fmt.Sprintf("Memory %s / %s", human(last.UsedMem), human(last.CapMem)), func(p Point) float64 { return float64(p.UsedMem) }, capMem},
		{fmt.Sprintf("Sessions %d  queue %d", last.Sessions, last.Queue), func(p Point) float64 { return float64(p.Sessions + p.Queue) }, sessQ},
		{fmt.Sprintf("Cost $%.2f/h  ($%.2f today)", s.Cost.PerHour, s.Cost.Total), func(p Point) float64 { return p.CostPerHour }, cost},
	}
	var panels []string
	for _, x := range gs {
		vals, ok := Series(h.Pool, at, x.val, s.Time, v.Window, inner)
		body := st.Spark.Render(Sparkline(vals, ok, x.top, 3))
		panels = append(panels, st.Panel.Width(inner).Render(st.Title.Render(fit(x.title, inner))+"\n"+body))
	}
	row1 := lipgloss.JoinHorizontal(lipgloss.Top, panels[0], panels[1])
	row2 := lipgloss.JoinHorizontal(lipgloss.Top, panels[2], panels[3])
	return row1 + "\n" + row2 + "\n"
}

// machines draws one line per machine: state, busy-core bar with history,
// memory bar, pressure, sessions and price.
func machines(s Snapshot, h *History, v View, st Styles) string {
	inner := v.Width - 2
	var lines []string
	ms := s.Up()
	for _, m := range ms {
		size := m.Labels[provider.LabelSize]
		if size == "" {
			size = "-"
		}
		state := string(m.State)
		busy, mem := 0.0, int64(0)
		psi, sess := "-", "-"
		if st0 := m.Status; st0 != nil {
			busy, mem = st0.Measured.CPU, st0.Measured.Memory
			psi = fmt.Sprintf("psi %.2f/%.2f", st0.Pressure.CPU, st0.Pressure.Memory)
			sess = fmt.Sprintf("%d sess", len(st0.Sessions))
			switch {
			case !st0.Ready:
				state = "provisioning"
			case st0.Draining || m.Draining:
				state = "draining"
			case len(st0.Sessions) == 0 && st0.IdleSince > 0:
				state = "idle " + short(s.Time.Sub(time.Unix(st0.IdleSince, 0)))
			}
		}
		head := fmt.Sprintf("%-16s %-20s %-12s", trunc(m.ID, 16), trunc(m.Provider+"/"+size, 20), trunc(state, 12))
		cpu := fmt.Sprintf("cpu %s %5.1f/%-3.0f", bar(busy, m.CPU, 10, st), busy, m.CPU)
		mp := h.Machine[m.ID]
		vals, ok := Series(mp, func(p MPoint) time.Time { return p.T }, func(p MPoint) float64 { return p.BusyCPU }, s.Time, v.Window, 16)
		spark := st.Spark.Render(Sparkline(vals, ok, m.CPU, 1))
		memS := fmt.Sprintf("mem %s %s/%s", bar(float64(mem), float64(m.Memory), 8, st), human(mem), human(m.Memory))
		tail := fmt.Sprintf("%-13s %-7s $%.2f/h", psi, sess, m.PricePerHour)
		line := head + " " + cpu + " " + spark + " " + memS + "  " + tail
		if lipgloss.Width(line) > inner {
			line = head + " " + cpu + " " + memS + "  " + tail
		}
		lines = append(lines, fit(line, inner))
	}
	if len(lines) == 0 {
		lines = []string{st.Dim.Render(fit("no machines", inner))}
	}
	title := st.Title.Render(fmt.Sprintf("Machines (%d)", len(ms)))
	return st.Panel.Width(inner).Render(title+"\n"+strings.Join(lines, "\n")) + "\n"
}

type col struct {
	name string
	w    int
	key  SortKey
	sort bool
}

// table draws the sortable session table; avail bounds the visible rows
// (0: all).
func table(rows []Row, v View, avail int, st Styles) string {
	inner := v.Width - 2
	cols := []col{
		{"MACHINE", 14, SortMachine, true}, {"SESSION", 8, 0, false}, {"AGENT", 11, SortAgent, true},
		{"WORKTREE", 0, SortWorktree, true}, {"BRANCH", 16, 0, false}, {"ELAPSED", 8, SortElapsed, true},
		{"CPU used/res", 12, SortCPU, true}, {"MEM used/res", 13, SortMem, true}, {"COMMAND", 0, 0, false},
	}
	fixed := 0
	for _, c := range cols {
		fixed += c.w + 1
	}
	flex := max(16, inner-fixed)
	cols[3].w = flex * 2 / 5
	cols[8].w = flex - cols[3].w - 1
	var hdr []string
	for _, c := range cols {
		n := c.name
		if c.sort && c.key == v.Sort {
			if v.Asc {
				n += "▲"
			} else {
				n += "▼"
			}
		}
		hdr = append(hdr, pad(n, c.w))
	}
	lines := []string{st.Header.Render(fit(strings.Join(hdr, " "), inner))}
	sel := min(max(v.Selected, 0), max(len(rows)-1, 0))
	off := v.Offset
	if avail > 0 {
		if sel < off {
			off = sel
		}
		if sel >= off+avail {
			off = sel - avail + 1
		}
		off = min(max(off, 0), max(len(rows)-avail, 0))
	} else {
		off = 0
	}
	for i := off; i < len(rows); i++ {
		if avail > 0 && i >= off+avail {
			break
		}
		r := rows[i]
		agent := r.Agent
		if agent == "" {
			agent = "-"
		}
		branch := r.Branch
		if branch == "" {
			branch = "-"
		}
		cells := []string{
			trunc(r.Machine, 14), trunc(r.Session, 8), trunc(agent, 11), truncLeft(shortPath(r.Worktree), cols[3].w),
			trunc(branch, 16), short(r.Elapsed),
			fmt.Sprintf("%.1f/%.1f", r.BusyCPU, r.ResCPU), human(r.UsedMem) + "/" + human(r.ResMem), trunc(r.Command, cols[8].w),
		}
		var parts []string
		for j, c := range cols {
			parts = append(parts, pad(cells[j], c.w))
		}
		line := fit(strings.Join(parts, " "), inner)
		if i == sel && v.Height > 0 {
			line = st.Selected.Render(line)
		}
		lines = append(lines, line)
	}
	if len(rows) == 0 {
		lines = append(lines, st.Dim.Render(fit("no running sessions", inner)))
	}
	title := st.Title.Render(fmt.Sprintf("Sessions (%d)  sort %s", len(rows), sortNames[v.Sort]))
	return st.Panel.Width(inner).Render(title+"\n"+strings.Join(lines, "\n")) + "\n"
}

func queue(s Snapshot, v View, st Styles) string {
	inner := v.Width - 2
	var lines []string
	for _, q := range s.Queue {
		agent := q.Labels[labels.Agent]
		if agent == "" {
			agent = "-"
		}
		wt := q.Labels[labels.Worktree]
		if wt == "" {
			wt = q.RepoID
		}
		lines = append(lines, fit(fmt.Sprintf("%-10s waiting %-7s %-11s %-30s need %.1f cpu %s  %s", trunc(q.ID, 10),
			short(s.Time.Sub(q.Since)), trunc(agent, 11), truncLeft(shortPath(wt), 30), q.Need.CPU, human(q.Need.Memory), q.Key), inner))
	}
	title := st.Warn.Render(fmt.Sprintf("Queue (%d)", len(s.Queue)))
	return st.Panel.Width(inner).Render(title+"\n"+strings.Join(lines, "\n")) + "\n"
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

// bar draws a filled bar of value/total.
func bar(value, total float64, w int, st Styles) string {
	frac := 0.0
	if total > 0 {
		frac = math.Min(1, math.Max(0, value/total))
	}
	n := int(math.Round(frac * float64(w)))
	return "[" + st.Bar.Render(strings.Repeat("█", n)) + st.BarBg.Render(strings.Repeat("░", w-n)) + "]"
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
