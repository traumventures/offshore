// Package top implements `offshore top`: a live view of the pool
// (machines, sessions, queue, cost) in the style of zenith, with history
// graphs built from status snapshots and machine_sample events kept in
// memory. Render produces one frame; the interactive program (tui.go)
// redraws it every refresh and handles keys. `--once` prints Render's
// plain text frame.
package top

import (
	"sort"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
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
	// Events are the pool events since the previous snapshot (finished
	// sessions, placements, machine state changes); nil when unknown.
	Events []events.Event
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
	T          time.Time
	BusyCPU    float64
	UsedMem    int64
	PSICPU     float64 // CPU pressure (some avg10, 0..1)
	PSIMem     float64
	DiskUsed   int64
	PageIn     float64 // cache file system page-in bytes/s since the previous point
	Writeback  float64 // writeback bytes/s
	pageInCum  int64
	writebkCum int64
}

// SPoint is one running session at one time.
type SPoint struct {
	T       time.Time
	BusyCPU float64
	UsedMem int64
}

// Finished is a session that ended (from task_end events).
type Finished struct {
	ID, Machine, Key string
	End              time.Time
	Wall             time.Duration
	Exit             int
	OOM, Warm        bool
	Reserve          scheduler.Resources
	CPUSeconds       float64
	PeakMem          int64
	Labels           map[string]string
	Usage            scheduler.Usage
}

// Placed is the placement decision of a session (task_placed).
type Placed struct {
	Reason, Placement, Machine string
	Arrival                    time.Time
	Wait                       time.Duration
	Need                       scheduler.Resources
}

// StateChange is one machine lifecycle event.
type StateChange struct {
	T            time.Time
	What, Reason string
}

// History keeps pool, machine and session points for at most Keep, plus
// the recently finished sessions, placements and machine lifecycles.
type History struct {
	Keep     time.Duration
	Pool     []Point
	Machine  map[string][]MPoint
	Session  map[string][]SPoint
	Finished []Finished // most recent last, at most maxFinished
	Placed   map[string]Placed
	States   map[string][]StateChange
}

const maxFinished = 200

// NewHistory returns an empty history keeping keep (0: 3h).
func NewHistory(keep time.Duration) *History {
	if keep <= 0 {
		keep = 3 * time.Hour
	}
	return &History{Keep: keep, Machine: map[string][]MPoint{}, Session: map[string][]SPoint{},
		Placed: map[string]Placed{}, States: map[string][]StateChange{}}
}

// Events records task and machine lifecycle events (finished sessions,
// placements, state changes); samples are ignored here (see Seed).
func (h *History) Events(evs []events.Event) {
	for _, ev := range evs {
		switch ev.Type {
		case events.TaskPlaced:
			p := Placed{Reason: ev.Reason, Placement: ev.Placement, Machine: ev.Machine, Arrival: ev.Arrival, Wait: time.Duration(ev.WaitMs) * time.Millisecond}
			if ev.Need != nil {
				p.Need = *ev.Need
			}
			h.Placed[ev.Task] = p
		case events.TaskEnd:
			if ev.Usage == nil {
				continue
			}
			f := Finished{ID: ev.Task, Machine: ev.Machine, Key: ev.Key, End: ev.Time, Wall: ev.Usage.Wall, Exit: ev.Usage.ExitCode,
				OOM: ev.Usage.OOM, Warm: ev.Usage.Warm, CPUSeconds: ev.Usage.CPUSeconds, PeakMem: ev.Usage.PeakMemory, Labels: ev.Labels, Usage: *ev.Usage}
			if ev.Reserve != nil {
				f.Reserve = *ev.Reserve
			}
			dup := false
			for _, x := range h.Finished {
				dup = dup || x.ID == f.ID
			}
			if !dup {
				h.Finished = append(h.Finished, f)
			}
		case events.MachineCreate, events.MachineReady, events.MachineStart, events.MachineStop, events.MachineDestroy,
			events.MachineDrain, events.MachineUndrain, events.MachineState:
			what := strings.TrimPrefix(ev.Type, "machine_")
			if ev.Type == events.MachineState {
				what = ev.State
			}
			sc := h.States[ev.Machine]
			if n := len(sc); n > 0 && sc[n-1].T.Equal(ev.Time) && sc[n-1].What == what {
				continue
			}
			h.States[ev.Machine] = append(sc, StateChange{T: ev.Time, What: what, Reason: ev.Reason})
		}
	}
	sort.SliceStable(h.Finished, func(i, j int) bool { return h.Finished[i].End.Before(h.Finished[j].End) })
	if n := len(h.Finished); n > maxFinished {
		h.Finished = append([]Finished(nil), h.Finished[n-maxFinished:]...)
	}
	for id, sc := range h.States {
		if len(sc) > 50 {
			h.States[id] = append([]StateChange(nil), sc[len(sc)-50:]...)
		}
	}
	if len(h.Placed) > 2*maxFinished {
		keep := map[string]Placed{}
		for _, f := range h.Finished {
			if p, ok := h.Placed[f.ID]; ok {
				keep[f.ID] = p
			}
		}
		h.Placed = keep
	}
}

// Add appends a snapshot (and records its events).
func (h *History) Add(s Snapshot) {
	h.Events(s.Events)
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
			mp.PSICPU, mp.PSIMem = st.Pressure.CPU, st.Pressure.Memory
			if st.Disk != nil {
				mp.DiskUsed = st.Disk.Used
			}
			if vm := st.VM; vm != nil {
				mp.pageInCum, mp.writebkCum = vm.PageInBytes, vm.WritebackBytes
				if prev := h.Machine[m.ID]; len(prev) > 0 {
					l := prev[len(prev)-1]
					if dt := s.Time.Sub(l.T).Seconds(); dt > 0 && l.pageInCum > 0 && vm.PageInBytes >= l.pageInCum && vm.WritebackBytes >= l.writebkCum {
						mp.PageIn = float64(vm.PageInBytes-l.pageInCum) / dt
						mp.Writeback = float64(vm.WritebackBytes-l.writebkCum) / dt
					}
				}
			}
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
	h.Events(evs)
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
		if ev.Pressure != nil {
			mp.PSICPU, mp.PSIMem = ev.Pressure.CPU, ev.Pressure.Memory
		}
		if ev.Disk != nil {
			mp.DiskUsed = ev.Disk.Used
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

// AddSessions records the busy cores (rates) and memory of running
// sessions.
func (h *History) AddSessions(s Snapshot, rates map[string]float64) {
	for _, r := range Rows(s, rates) {
		h.Session[r.Session] = append(h.Session[r.Session], SPoint{T: s.Time, BusyCPU: r.BusyCPU, UsedMem: r.UsedMem})
	}
	cut := s.Time.Add(-h.Keep)
	for id, ps := range h.Session {
		if len(ps) == 0 || ps[len(ps)-1].T.Before(cut) {
			delete(h.Session, id)
		}
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
