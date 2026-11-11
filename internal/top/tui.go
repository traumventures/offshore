package top

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/traumventures/cloudsourced/internal/events"
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
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
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
		}
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
	case tickMsg:
		return m, m.poll()
	case seedMsg:
		m.hist.Seed(msg)
	case tea.WindowSizeMsg:
		m.view.Width, m.view.Height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

// key applies one key press to the view; it returns tea.Quit to exit.
func (m *model) key(k string) tea.Cmd {
	v := &m.view
	n := len(Rows(m.snap, m.cur))
	page := max(1, v.Height/3)
	switch k {
	case "q", "esc", "ctrl+c":
		return tea.Quit
	case "up", "k":
		v.Selected--
	case "down", "j":
		v.Selected++
	case "pgup":
		v.Selected -= page
	case "pgdown":
		v.Selected += page
	case "home", "g":
		v.Selected = 0
	case "end", "G":
		v.Selected = n - 1
	case "+", "=":
		v.Window = zoom(v.Window, -1)
	case "-", "_":
		v.Window = zoom(v.Window, 1)
	case "r":
		v.Asc = !v.Asc
	default:
		keys := map[string]SortKey{"e": SortElapsed, "c": SortCPU, "m": SortMem, "a": SortAgent, "w": SortWorktree, "n": SortMachine}
		if s, ok := keys[k]; ok {
			if v.Sort == s {
				v.Asc = !v.Asc
			} else {
				v.Sort, v.Asc = s, s == SortAgent || s == SortWorktree || s == SortMachine
			}
		}
	}
	v.Selected = min(max(v.Selected, 0), max(n-1, 0))
	return nil
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
			return "cloudsourced top: " + m.err.Error() + "\n(retrying; q to quit)"
		}
		return "cloudsourced top: loading…"
	}
	f := Frame(m.snap, m.hist, m.view, m.cur, m.styles)
	if m.err != nil {
		f = m.styles.Warn.Render(fit("refresh failed: "+m.err.Error(), m.view.Width)) + "\n" + f
	}
	return f
}
