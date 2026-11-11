// Package top implements `offshore top`: a live view of the pool
// (machines, sessions, queue, cost) in the style of zenith, with history
// graphs built from status snapshots and machine_sample events kept in
// memory. Render produces one frame; the interactive program (tui.go)
// redraws it every refresh and handles keys. `--once` prints Render's
// plain text frame.
package top

import (
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/provider"
)

// Snapshot is one poll of the pool.
type Snapshot struct {
	Time     time.Time
	Machines []orchestrator.Machine // as returned by status (incl. removed)
	Queue    []orchestrator.QueuedTask
	// QueueKnown is false when the pool cannot list its queue (older
	// control plane).
	QueueKnown bool
	Cost       orchestrator.CostReport // since the start of the day
}

// Up returns the machines that are not removed, by id.
func (s Snapshot) Up() []orchestrator.Machine {
	var out []orchestrator.Machine
	for _, m := range s.Machines {
		if m.Removed.IsZero() {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// running reports whether a machine is up (costs money).
func running(m orchestrator.Machine) bool {
	return m.Removed.IsZero() && (m.State == provider.Running || m.State == provider.Starting)
}

// Point is the pool at one time.
type Point struct {
	T           time.Time
	BusyCPU     float64 // measured busy cores
	ReservedCPU float64
	CapCPU      float64
	UsedMem     int64 // measured session memory
	CapMem      int64
	Sessions    int
	Queue       int
	CostPerHour float64
}

// MPoint is one machine at one time.
type MPoint struct {
	T       time.Time
	BusyCPU float64
	UsedMem int64
}

// History keeps pool and machine points for at most Keep.
type History struct {
	Keep    time.Duration
	Pool    []Point
	Machine map[string][]MPoint
}

// NewHistory returns an empty history keeping keep (0: 3h).
func NewHistory(keep time.Duration) *History {
	if keep <= 0 {
		keep = 3 * time.Hour
	}
	return &History{Keep: keep, Machine: map[string][]MPoint{}}
}

// Add appends a snapshot.
func (h *History) Add(s Snapshot) {
	p := Point{T: s.Time, Queue: len(s.Queue), CostPerHour: s.Cost.PerHour}
	for _, m := range s.Up() {
		if !running(m) {
			continue
		}
		p.CapCPU += m.CPU
		p.CapMem += m.Memory
		mp := MPoint{T: s.Time}
		if st := m.Status; st != nil {
			p.BusyCPU += st.Measured.CPU
			p.UsedMem += st.Measured.Memory
			p.ReservedCPU += st.UsedCPU
			p.Sessions += len(st.Sessions)
			mp.BusyCPU, mp.UsedMem = st.Measured.CPU, st.Measured.Memory
		}
		h.Machine[m.ID] = append(h.Machine[m.ID], mp)
	}
	h.Pool = append(h.Pool, p)
	h.trim(s.Time)
}

// Seed prepends the pool history recorded in machine_sample events (every
// events.SampleInterval per running machine). Snapshots added later
// continue it. The queue is not sampled and stays 0 in seeded points.
func (h *History) Seed(evs []events.Event) {
	// One sample per machine and bucket (the last): samples are taken
	// about every SampleInterval, so two can fall into one bucket and
	// would count the machine's capacity twice.
	type key struct {
		t  int64
		id string
	}
	last := map[key]events.Event{}
	for _, ev := range evs {
		if ev.Type != events.MachineSample {
			continue
		}
		k := key{ev.Time.Truncate(events.SampleInterval).Unix(), ev.Machine}
		if old, ok := last[k]; !ok || !ev.Time.Before(old.Time) {
			last[k] = ev
		}
	}
	buckets := map[int64]*Point{}
	seeded := map[string][]MPoint{}
	for k, ev := range last {
		t := time.Unix(k.t, 0)
		b := buckets[k.t]
		if b == nil {
			b = &Point{T: t}
			buckets[k.t] = b
		}
		mp := MPoint{T: t}
		if ev.Capacity != nil {
			b.CapCPU += ev.Capacity.CPU
			b.CapMem += ev.Capacity.Memory
		}
		if ev.Used != nil {
			b.BusyCPU += ev.Used.CPU
			b.UsedMem += ev.Used.Memory
			mp.BusyCPU, mp.UsedMem = ev.Used.CPU, ev.Used.Memory
		}
		if ev.Reserved != nil {
			b.ReservedCPU += ev.Reserved.CPU
		}
		b.Sessions += ev.Sessions
		b.CostPerHour += ev.Price
		seeded[ev.Machine] = append(seeded[ev.Machine], mp)
	}
	pts := make([]Point, 0, len(buckets))
	for _, b := range buckets {
		pts = append(pts, *b)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T.Before(pts[j].T) })
	first := func(t time.Time) bool { return len(h.Pool) == 0 || t.Before(h.Pool[0].T) }
	var keep []Point
	for _, p := range pts {
		if first(p.T) {
			keep = append(keep, p)
		}
	}
	h.Pool = append(keep, h.Pool...)
	for id, ms := range seeded {
		sort.Slice(ms, func(i, j int) bool { return ms[i].T.Before(ms[j].T) })
		cur := h.Machine[id]
		var k []MPoint
		for _, p := range ms {
			if len(cur) == 0 || p.T.Before(cur[0].T) {
				k = append(k, p)
			}
		}
		h.Machine[id] = append(k, cur...)
	}
	if n := len(h.Pool); n > 0 {
		h.trim(h.Pool[n-1].T)
	}
}

func (h *History) trim(now time.Time) {
	cut := now.Add(-h.Keep)
	i := 0
	for i < len(h.Pool) && h.Pool[i].T.Before(cut) {
		i++
	}
	h.Pool = h.Pool[i:]
	for id, ms := range h.Machine {
		j := 0
		for j < len(ms) && ms[j].T.Before(cut) {
			j++
		}
		if j == len(ms) {
			delete(h.Machine, id)
			continue
		}
		h.Machine[id] = ms[j:]
	}
}

// Series resamples points over (now-window, now] into n columns, each the
// last value that falls into it. Gaps after the first point repeat the
// previous column; ok marks columns with data.
func Series[T any](pts []T, at func(T) time.Time, val func(T) float64, now time.Time, window time.Duration, n int) (vals []float64, ok []bool) {
	vals, ok = make([]float64, max(n, 0)), make([]bool, max(n, 0))
	if n <= 0 || window <= 0 {
		return vals, ok
	}
	start := now.Add(-window)
	step := window / time.Duration(n)
	if step <= 0 {
		step = time.Nanosecond
	}
	for _, p := range pts {
		t := at(p)
		if !t.After(start) || t.After(now) {
			continue
		}
		c := min(int(t.Sub(start)/step), n-1)
		vals[c], ok[c] = val(p), true
	}
	seen := false
	for i := range vals {
		if ok[i] {
			seen = true
		} else if seen {
			vals[i], ok[i] = vals[i-1], true
		}
	}
	return vals, ok
}
