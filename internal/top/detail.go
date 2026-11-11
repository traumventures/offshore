package top

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
)

// detail renders the open detail view (machine or session).
func detail(s Snapshot, h *History, v View, rates map[string]float64, st Styles) string {
	kind, id, _ := strings.Cut(v.Detail, ":")
	var body string
	switch kind {
	case "machine":
		body = machineDetail(s, h, v, rates, st, id)
	case "session":
		body = sessionDetail(s, h, v, rates, st, id)
	case "storage":
		body = storageDetail(s, h, v, st, id)
	}
	return header(s, v, st) + "\n" + body + st.Dim.Render(fit("Esc/q back  ↑/↓ other rows  +/- window", v.Width))
}

// kv renders "key value" lines in two columns where the width allows.
func kv(pairs [][2]string, width int) []string {
	kw := 0
	for _, p := range pairs {
		kw = max(kw, len(p[0]))
	}
	var cells []string
	cw := 0
	for _, p := range pairs {
		c := fmt.Sprintf("%-*s  %s", kw, p[0], p[1])
		cells = append(cells, c)
		cw = max(cw, lipgloss.Width(c))
	}
	cols := 1
	if cw > 0 && width >= 2*cw+4 {
		cols = 2
	}
	var lines []string
	rows := (len(cells) + cols - 1) / cols
	for r := 0; r < rows; r++ {
		var parts []string
		for c := 0; c < cols; c++ {
			if i := c*rows + r; i < len(cells) {
				parts = append(parts, pad(cells[i], cw))
			}
		}
		lines = append(lines, fit(strings.Join(parts, "    "), width))
	}
	return lines
}

// graphGrid draws graphs two per row, rows high.
func graphGrid(gs []graphSpec, width, rows int, st Styles) string {
	var out []string
	for i := 0; i < len(gs); i += 2 {
		var panels []string
		n := min(2, len(gs)-i)
		for j := 0; j < n; j++ {
			pw := width/n - 2
			if j == n-1 {
				pw = width - (width/n)*(n-1) - 2
			}
			g := gs[i+j]
			vals, ok := g.series(pw)
			panels = append(panels, st.Panel.Width(pw).Render(st.Title.Render(fit(g.title, pw))+"\n"+st.Spark.Render(Sparkline(vals, ok, g.top, rows))))
		}
		out = append(out, lipgloss.JoinHorizontal(lipgloss.Top, panels...))
	}
	return strings.Join(out, "\n") + "\n"
}

type graphSpec struct {
	title  string
	top    float64
	series func(n int) ([]float64, []bool)
}

// detailGraphRows is the sparkline height of gridRows rows of graph
// panels when the other lines of the screen take other lines.
func detailGraphRows(height, other, gridRows int) int {
	if height <= 0 {
		return 3
	}
	return min(max((height-other)/max(gridRows, 1)-3, 1), 10)
}

func findMachine(s Snapshot, id string) (orchestrator.Machine, bool) {
	for _, m := range s.Machines {
		if m.ID == id {
			return m, true
		}
	}
	return orchestrator.Machine{}, false
}

func machineDetail(s Snapshot, h *History, v View, rates map[string]float64, st Styles, id string) string {
	inner := v.Width - 2
	m, ok := findMachine(s, id)
	if !ok {
		return panel(st.Title.Render("Machine "+id), []string{st.Dim.Render("machine gone")}, 0, inner, st)
	}
	size := m.Labels[provider.LabelSize]
	env := m.EnvHash
	if len(env) > 12 {
		env = env[:12]
	}
	expires := "-"
	if !m.ExpiresAt.IsZero() {
		expires = short(m.ExpiresAt.Sub(s.Time))
	}
	day := orchestrator.StartOfDay(s.Time)
	age := "-"
	if !m.CreatedAt.IsZero() {
		age = short(s.Time.Sub(m.CreatedAt))
	}
	pairs := [][2]string{
		{"provider", m.Provider + "/" + size}, {"state", machineState(m, s.Time)}, {"zone", dashS(m.Zone)}, {"address", dashS(m.Addr)},
		{"environment", dashS(env)}, {"age", age}, {"expires in", expires},
		{"price", fmt.Sprintf("$%.3f/h", m.PricePerHour)},
		{"cost today", fmt.Sprintf("$%.2f (up %s)", m.Cost(day, s.Time), short(m.UpTime(day, s.Time).Round(time.Second)))},
		{"capacity", fmt.Sprintf("%g cpu, %s", m.CPU, human(m.Memory))},
	}
	if st0 := m.Status; st0 != nil {
		pairs = append(pairs, [2]string{"reserved", fmt.Sprintf("%.1f cpu, %s", st0.UsedCPU, human(st0.UsedMemory))},
			[2]string{"pressure", fmt.Sprintf("cpu %.2f  mem %.2f", st0.Pressure.CPU, st0.Pressure.Memory)})
		if d := st0.Disk; d != nil {
			pairs = append(pairs, [2]string{"disk", fmt.Sprintf("%s/%s (slots %s, machine caches %s, budget %s)", human(d.Used), human(d.Total), human(d.Slots), human(d.MachineCaches), human(d.Budget))})
		}
		if vm := st0.VM; vm != nil {
			pairs = append(pairs, [2]string{"cache fs", vmLine(*vm)})
		}
	}
	var hist []string
	for _, sc := range h.States[id] {
		t := sc.T.Local().Format("15:04:05") + " " + sc.What
		if sc.Reason != "" {
			t += " (" + sc.Reason + ")"
		}
		hist = append(hist, t)
	}
	if len(hist) > 6 {
		hist = hist[len(hist)-6:]
	}
	if len(hist) > 0 {
		pairs = append(pairs, [2]string{"history", strings.Join(hist, ", ")})
	}
	info := kv(pairs, inner)
	// Slots on the machine.
	var slots []string
	if m.Status != nil && m.Status.Disk != nil {
		for _, r := range storageRows(s) {
			if r.Machine == id && !r.Header && r.Item.Kind == proto.StorageSlot {
				it := r.Item
				slots = append(slots, fmt.Sprintf("%-9s %s  committed %s  store %s  lag %s", hum0(it.Bytes), itemName(it), ago(s.Time, it.Committed), hum0(it.Stored), storeLag(it)))
			}
		}
	}
	// Sessions running and recently on it.
	var sess []string
	for _, r := range Rows(s, rates) {
		if r.Machine == id {
			sess = append(sess, fmt.Sprintf("running  %-8s %-7s %4.1f/%-4.1f cpu %s/%s  %s  %s", trunc(r.Session, 8), short(r.Elapsed), r.BusyCPU, r.ResCPU,
				human(r.UsedMem), human(r.ResMem), dashS(r.Client), r.Command))
		}
	}
	for i := len(h.Finished) - 1; i >= 0 && len(sess) < 8; i-- {
		f := h.Finished[i]
		if f.Machine == id {
			sess = append(sess, fmt.Sprintf("exit %-3d %-8s %-7s %s ago  %s  %s", f.Exit, trunc(f.ID, 8), short(f.Wall), short(s.Time.Sub(f.End)),
				dashS(labels.UserHost(f.Labels)), keyCommand(f.Key)))
		}
	}
	mp := h.Machine[id]
	at := func(p MPoint) time.Time { return p.T }
	ser := func(f func(MPoint) float64) func(int) ([]float64, []bool) {
		return func(n int) ([]float64, []bool) { return Series(mp, at, f, s.Time, v.Window, n) }
	}
	last := MPoint{}
	if len(mp) > 0 {
		last = mp[len(mp)-1]
	}
	disk := 0.0
	if m.Status != nil && m.Status.Disk != nil {
		disk = float64(m.Status.Disk.Total)
	}
	gs := []graphSpec{
		{fmt.Sprintf("CPU busy %.1f / %g", last.BusyCPU, m.CPU), m.CPU, ser(func(p MPoint) float64 { return p.BusyCPU })},
		{fmt.Sprintf("Memory %s / %s", human(last.UsedMem), human(m.Memory)), float64(m.Memory), ser(func(p MPoint) float64 { return float64(p.UsedMem) })},
		{fmt.Sprintf("Pressure cpu %.2f mem %.2f", last.PSICPU, last.PSIMem), 1, ser(func(p MPoint) float64 { return math.Max(p.PSICPU, p.PSIMem) })},
		{fmt.Sprintf("Disk %s  page-in %s/s  writeback %s/s", human(last.DiskUsed), human(int64(last.PageIn)), human(int64(last.Writeback))), disk,
			ser(func(p MPoint) float64 { return float64(p.DiskUsed) })},
	}
	other := 1 + len(info) + 3 + max(len(slots), 1) + 3 + max(len(sess), 1) + 3 + 1
	rows := detailGraphRows(v.Height, other, 2)
	var b strings.Builder
	b.WriteString(panel(st.Title.Render(fit("Machine "+id, inner)), info, 0, inner, st))
	b.WriteString(graphGrid(gs, v.Width, rows, st))
	if len(slots) == 0 {
		slots = []string{st.Dim.Render("no build slots")}
	}
	if len(sess) == 0 {
		sess = []string{st.Dim.Render("no sessions")}
	}
	b.WriteString(panel(st.Title.Render("Build slots"), fitAll(slots, inner), 0, inner, st))
	b.WriteString(panel(st.Title.Render("Sessions"), fitAll(sess, inner), 0, inner, st))
	return b.String()
}

func fitAll(ls []string, w int) []string {
	for i := range ls {
		ls[i] = fit(ls[i], w)
	}
	return ls
}

func dashS(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// vmLine summarises a machine's cache file system.
func vmLine(vm proto.VMStats) string {
	res := human(vm.ResidentBytes)
	if vm.Capacity > 0 {
		res += "/" + human(vm.Capacity)
	}
	out := fmt.Sprintf("resident %s  dirty %s  working set %s  %.1f faults/s  page-in %s  writeback %s total", res, human(vm.DirtyBytes),
		human(vm.WorkingSetBytes), vm.FaultRate, human(vm.PageInBytes), human(vm.WritebackBytes))
	if vm.Thrashing {
		out += "  THRASH"
	}
	return out
}

func sessionDetail(s Snapshot, h *History, v View, rates map[string]float64, st Styles, id string) string {
	inner := v.Width - 2
	var row *Row
	state := ""
	for _, r := range Rows(s, rates) {
		if r.Session == id {
			r := r
			row, state = &r, "running"
		}
	}
	var fin *Finished
	if row == nil {
		for i := range h.Finished {
			if h.Finished[i].ID == id {
				fin = &h.Finished[i]
			}
		}
	}
	queued := false
	if row == nil && fin == nil {
		for _, q := range s.Queue {
			if q.ID == id {
				row = &Row{Session: q.ID, Worktree: q.Labels[labels.Worktree], Branch: q.Labels[labels.Branch], Client: labels.UserHost(q.Labels),
					Command: keyCommand(q.Key), Labels: q.Labels, Elapsed: s.Time.Sub(q.Since), ResCPU: q.Need.CPU, ResMem: q.Need.Memory}
				state, queued = "queued", true
			}
		}
	}
	if row == nil && fin == nil {
		return panel(st.Title.Render("Session "+id), []string{st.Dim.Render("session not found")}, 0, inner, st)
	}
	var lbl map[string]string
	var pairs [][2]string
	machine := ""
	if row != nil {
		lbl, machine = row.Labels, row.Machine
		pairs = append(pairs, [2]string{"command", row.Command}, [2]string{"state", state}, [2]string{"elapsed", short(row.Elapsed)},
			[2]string{"reserved", fmt.Sprintf("%.1f cpu, %s", row.ResCPU, human(row.ResMem))})
		if !queued {
			pairs = append(pairs, [2]string{"usage", fmt.Sprintf("%.1f cores, %s", row.BusyCPU, human(row.UsedMem))})
		}
	} else {
		lbl, machine = fin.Labels, fin.Machine
		u := fin.Usage
		ex := fmt.Sprint(fin.Exit)
		if fin.OOM {
			ex += " (OOM)"
		}
		cores := 0.0
		if fin.Wall > 0 {
			cores = fin.CPUSeconds / fin.Wall.Seconds()
		}
		pairs = append(pairs, [2]string{"command", keyCommand(fin.Key)}, [2]string{"state", "finished " + short(s.Time.Sub(fin.End)) + " ago"},
			[2]string{"exit code", ex}, [2]string{"duration", short(fin.Wall)},
			[2]string{"reserved", fmt.Sprintf("%.1f cpu, %s", fin.Reserve.CPU, human(fin.Reserve.Memory))},
			[2]string{"usage", fmt.Sprintf("%.1f cores avg (%.0f cpu s), peak %s", cores, fin.CPUSeconds, human(fin.PeakMem))},
			[2]string{"build slot", fmt.Sprintf("warm %v restored %v  %s -> %s  working set %s", u.Warm, u.Restored, human(u.SlotBytesBefore), human(u.SlotBytesAfter), human(u.WorkingSetBytes))},
			[2]string{"stalls", fmt.Sprintf("cpu %.1fs  mem %.1fs", u.CPUStall, u.MemoryStall)})
	}
	pairs = append(pairs, [2]string{"client", dashS(labels.UserHost(lbl))}, [2]string{"worktree", dashS(shortPath(lbl[labels.Worktree]))},
		[2]string{"branch", dashS(lbl[labels.Branch])}, [2]string{"repo", dashS(lbl[labels.Repo])}, [2]string{"agent", dashS(lbl[labels.Agent])})
	if l := labels.Format(lbl); l != "" {
		pairs = append(pairs, [2]string{"labels", l})
	}
	if machine != "" {
		ms := machine
		if m, ok := findMachine(s, machine); ok {
			ms += fmt.Sprintf(" (%s/%s, $%.3f/h)", m.Provider, m.Labels[provider.LabelSize], m.PricePerHour)
			if m.Status != nil && m.Status.VM != nil {
				pairs = append(pairs, [2]string{"cache fs", vmLine(*m.Status.VM)})
			}
			if mp := h.Machine[machine]; len(mp) > 0 {
				l := mp[len(mp)-1]
				pairs = append(pairs, [2]string{"io", fmt.Sprintf("page-in %s/s  writeback %s/s (machine)", human(int64(l.PageIn)), human(int64(l.Writeback)))})
			}
		}
		pairs = append(pairs, [2]string{"machine", ms})
	}
	if p, ok := h.Placed[id]; ok {
		pairs = append(pairs, [2]string{"placement", fmt.Sprintf("%s: %s (waited %s, need %.1f cpu %s)", dashS(p.Placement), dashS(p.Reason), short(p.Wait.Round(time.Second)), p.Need.CPU, human(p.Need.Memory))})
	}
	info := kv(pairs, inner)
	var b strings.Builder
	b.WriteString(panel(st.Title.Render(fit("Session "+id, inner)), info, 0, inner, st))
	if sp := h.Session[id]; len(sp) > 0 {
		at := func(p SPoint) time.Time { return p.T }
		last := sp[len(sp)-1]
		topC, topM := 0.0, 0.0
		if row != nil {
			topC, topM = row.ResCPU, float64(row.ResMem)
		}
		resC, resM := topC, topM
		// A session may burst above its reservation; never clip the peak.
		for _, p := range sp {
			if p.T.After(s.Time.Add(-v.Window)) {
				topC = max(topC, p.BusyCPU)
				topM = max(topM, float64(p.UsedMem))
			}
		}
		gs := []graphSpec{
			{fmt.Sprintf("CPU %.1f cores (reserved %.1f)", last.BusyCPU, resC), topC, func(n int) ([]float64, []bool) {
				return Series(sp, at, func(p SPoint) float64 { return p.BusyCPU }, s.Time, v.Window, n)
			}},
			{fmt.Sprintf("Memory %s (reserved %s)", human(last.UsedMem), human(int64(resM))), topM, func(n int) ([]float64, []bool) {
				return Series(sp, at, func(p SPoint) float64 { return float64(p.UsedMem) }, s.Time, v.Window, n)
			}},
		}
		b.WriteString(graphGrid(gs, v.Width, detailGraphRows(v.Height, 1+len(info)+3+1, 1), st))
	}
	return b.String()
}
