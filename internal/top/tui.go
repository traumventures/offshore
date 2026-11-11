package top

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/traumventures/offshore/internal/events"
)

// Fetcher polls the pool.
type Fetcher func(ctx context.Context) (Snapshot, error)

// Seeder returns recent events (machine samples) to prefill the graphs.
type Seeder func(ctx context.Context, since time.Time) ([]events.Event, error)

// cpuMark is a session's CPU time at a poll.
type cpuMark struct {
	t   time.Time
	cpu float64
}

// Rates tracks busy cores per session from CPU-second deltas between polls.
type Rates struct {
	prev  map[string]cpuMark
	rates map[string]float64
}

// Update records a snapshot and returns the current rates by session.
func (r *Rates) Update(s Snapshot) map[string]float64 {
	if r.prev == nil {
		r.prev = map[string]cpuMark{}
	}
	next := map[string]cpuMark{}
	rates := map[string]float64{}
	for _, m := range s.Up() {
		if m.Status == nil {
			continue
		}
		for _, se := range m.Status.Sessions {
			cur := cpuMark{t: s.Time, cpu: se.CPUSeconds}
			next[se.ID] = cur
			if p, ok := r.prev[se.ID]; ok && cur.t.After(p.t) && cur.cpu >= p.cpu {
				rates[se.ID] = (cur.cpu - p.cpu) / cur.t.Sub(p.t).Seconds()
			} else if old, ok := r.rates[se.ID]; ok {
				rates[se.ID] = old
			}
		}
	}
	r.prev, r.rates = next, rates
	return rates
}

type snapMsg struct {
	s   Snapshot
	err error
}
type tickMsg struct{}
type seedMsg []events.Event

type model struct {
	fetch    Fetcher
	seed     Seeder
	interval time.Duration
	styles   Styles
	snap     Snapshot
	have     bool
	hist     *History
	rates    Rates
	cur      map[string]float64
	view     View
	err      error
}

// Run starts the interactive view, refreshing every interval.
func Run(fetch Fetcher, seed Seeder, interval time.Duration) error {
	m := &model{fetch: fetch, seed: seed, interval: interval, hist: NewHistory(0), view: DefaultView(),
		styles: NewStyles(lipgloss.DefaultRenderer())}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func (m *model) poll() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := m.fetch(ctx)
		return snapMsg{s, err}
	}
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.poll()}
	if m.seed != nil {
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			evs, err := m.seed(ctx, time.Now().Add(-m.hist.Keep))
			if err != nil {
				return nil // graphs start empty
			}
			return seedMsg(evs)
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case snapMsg:
		m.err = msg.err
		if msg.err == nil {
			m.snap, m.have = msg.s, true
			m.hist.Add(msg.s)
			m.cur = m.rates.Update(msg.s)
			m.hist.AddSessions(msg.s, m.cur)
			m.clamp()
		}
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
	case tickMsg:
		return m, m.poll()
	case seedMsg:
		m.hist.Seed(msg)
	case tea.WindowSizeMsg:
		m.view.Width, m.view.Height = msg.Width, msg.Height
	case tea.MouseMsg:
		return m, m.mouse(tea.MouseEvent(msg))
	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

// counts returns the rows of the session and machine tables.
func (m *model) counts() (sessions, machines int) {
	return len(sessionRows(m.snap, m.hist, m.view, m.cur)), len(m.snap.Up())
}

func (m *model) clamp() {
	n, k := m.counts()
	v := &m.view
	v.Selected = min(max(v.Selected, 0), max(n-1, 0))
	v.MSelected = min(max(v.MSelected, 0), max(k-1, 0))
	v.SSelected = min(max(v.SSelected, 0), max(len(storageRows(m.snap))-1, 0))
}

// selectedID returns the detail target of the focused table's selection.
func (m *model) selectedID() string {
	if m.view.Focus == FocusStorage {
		rows := storageRows(m.snap)
		if i := m.view.SSelected; i >= 0 && i < len(rows) {
			return "storage:" + rows[i].ID()
		}
		return ""
	}
	if m.view.Focus == FocusMachines {
		ms := Machines(m.snap, m.view)
		if i := m.view.MSelected; i >= 0 && i < len(ms) {
			return "machine:" + ms[i].ID
		}
		return ""
	}
	rows := sessionRows(m.snap, m.hist, m.view, m.cur)
	if i := m.view.Selected; i >= 0 && i < len(rows) {
		return "session:" + rows[i].Row.Session
	}
	return ""
}

// key applies one key press to the view; it returns tea.Quit to exit.
func (m *model) key(k string) tea.Cmd {
	v := &m.view
	if v.Detail != "" {
		switch k {
		case "esc", "q", "backspace", "enter":
			v.Detail = ""
		case "ctrl+c":
			return tea.Quit
		case "up", "k", "down", "j":
			m.move(map[string]int{"up": -1, "k": -1, "down": 1, "j": 1}[k])
			v.Detail = m.selectedID()
		case "+", "=":
			v.Window = zoom(v.Window, -1)
		case "-", "_":
			v.Window = zoom(v.Window, 1)
		}
		return nil
	}
	page := max(1, v.Height/3)
	switch k {
	case "q", "esc", "ctrl+c":
		return tea.Quit
	case "tab", "shift+tab":
		// Sessions -> machines -> storage (when shown) -> sessions.
		order := []Focus{FocusSessions, FocusMachines}
		if len(storageRows(m.snap)) > 0 {
			order = append(order, FocusStorage)
		}
		i := 0
		for i < len(order) && order[i] != v.Focus {
			i++
		}
		d := 1
		if k == "shift+tab" {
			d = len(order) - 1
		}
		v.Focus = order[(i+d)%len(order)]
	case "enter":
		v.Detail = m.selectedID()
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-page)
	case "pgdown":
		m.move(page)
	case "home", "g":
		m.move(-1 << 20)
	case "end", "G":
		m.move(1 << 20)
	case "+", "=":
		v.Window = zoom(v.Window, -1)
	case "-", "_":
		v.Window = zoom(v.Window, 1)
	case "r":
		if v.Focus == FocusMachines {
			v.MAsc = !v.MAsc
		} else {
			v.Asc = !v.Asc
		}
	default:
		if v.Focus == FocusStorage {
			break
		}
		if v.Focus == FocusMachines {
			keys := map[string]MSortKey{"s": MSortState, "c": MSortCPU, "m": MSortMem, "d": MSortDisk, "p": MSortPrice, "a": MSortAge, "n": MSortName}
			if s, ok := keys[k]; ok {
				if v.MSort == s {
					v.MAsc = !v.MAsc
				} else {
					v.MSort, v.MAsc = s, s == MSortName
				}
			}
			break
		}
		keys := map[string]SortKey{"e": SortElapsed, "c": SortCPU, "m": SortMem, "w": SortWorktree, "n": SortMachine, "u": SortUser}
		if s, ok := keys[k]; ok {
			if v.Sort == s {
				v.Asc = !v.Asc
			} else {
				v.Sort, v.Asc = s, s == SortWorktree || s == SortMachine || s == SortUser
			}
		}
	}
	m.clamp()
	return nil
}

// move moves the selection of the focused table by d rows.
func (m *model) move(d int) {
	switch m.view.Focus {
	case FocusMachines:
		m.view.MSelected += d
	case FocusStorage:
		m.view.SSelected += d
	default:
		m.view.Selected += d
	}
	m.clamp()
}

// mouse selects the clicked table row (a second click on the selected
// row opens it); the wheel moves the selection.
func (m *model) mouse(e tea.MouseEvent) tea.Cmd {
	v := &m.view
	switch {
	case e.Button == tea.MouseButtonWheelUp:
		m.move(-1)
		return nil
	case e.Button == tea.MouseButtonWheelDown:
		m.move(1)
		return nil
	case e.Button != tea.MouseButtonLeft || e.Action != tea.MouseActionPress:
		return nil
	}
	if v.Detail != "" {
		return nil
	}
	y := e.Y
	if m.err != nil {
		y-- // the refresh warning line
	}
	focus, idx, ok := HitTest(m.snap, m.hist, *v, m.cur, y)
	if !ok {
		return nil
	}
	cur := map[Focus]*int{FocusSessions: &v.Selected, FocusMachines: &v.MSelected, FocusStorage: &v.SSelected}[focus]
	same := focus == v.Focus && idx == *cur
	v.Focus = focus
	*cur = idx
	m.clamp()
	if same {
		v.Detail = m.selectedID()
	}
	return nil
}

// HitTest maps a screen line of a frame to a table row (focus and row
// index); ok is false outside the table rows.
func HitTest(s Snapshot, h *History, v View, rates map[string]float64, y int) (Focus, int, bool) {
	v = normalize(v)
	ms := Machines(s, v)
	srows := sessionRows(s, h, v, rates)
	stor := storageRows(s)
	l := ComputeLayout(v.Height, len(ms), len(stor), len(srows))
	mTop := 1 + l.Graph + 3 + 3 // header, graph panels, machine panel border/title/column header
	if y >= mTop && y < mTop+l.Machines {
		off := window(min(max(v.MSelected, 0), max(len(ms)-1, 0)), v.MOffset, l.Machines, len(ms))
		if i := off + y - mTop; i < len(ms) {
			return FocusMachines, i, true
		}
		return 0, 0, false
	}
	sTop := mTop + l.Machines + 1
	if l.Storage > 0 {
		stTop := sTop + 3
		if y >= stTop && y < stTop+l.Storage {
			off := window(min(max(v.SSelected, 0), max(len(stor)-1, 0)), v.SOffset, l.Storage, len(stor))
			if i := off + y - stTop; i < len(stor) {
				return FocusStorage, i, true
			}
			return 0, 0, false
		}
		sTop += l.Storage + storageFixed
	}
	sTop += 3
	if y >= sTop && y < sTop+l.Sessions {
		off := window(min(max(v.Selected, 0), max(len(srows)-1, 0)), v.Offset, l.Sessions, len(srows))
		if i := off + y - sTop; i < len(srows) {
			return FocusSessions, i, true
		}
	}
	return 0, 0, false
}

// zoom moves to the next shorter (dir < 0, "+") or longer window.
func zoom(cur time.Duration, dir int) time.Duration {
	i := 0
	for i < len(Windows)-1 && Windows[i] < cur {
		i++
	}
	return Windows[min(max(i+dir, 0), len(Windows)-1)]
}

func (m *model) View() string {
	if !m.have {
		if m.err != nil {
			return "offshore top: " + m.err.Error() + "\n(retrying; q to quit)"
		}
		return "offshore top: loading…"
	}
	f := Frame(m.snap, m.hist, m.view, m.cur, m.styles)
	if m.err != nil {
		f = m.styles.Warn.Render(fit("refresh failed: "+m.err.Error(), m.view.Width)) + "\n" + f
	}
	return f
}
