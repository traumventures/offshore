// Package report turns the orchestrator's analysis event log into a cost
// and efficiency report (`cloudsourced report`) and into simulator traces
// (`cloudsourced sim export`).
package report

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/provider"
)

// Lookback is how far before a report window machine events are needed to
// know which machines were up at its start.
const Lookback = 48 * time.Hour

// Report summarises a time window.
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Tasks is the number of finished sessions with reported usage.
	Tasks int `json:"tasks"`
	// Placements counts requests by outcome (assign, reject, timeout, ...).
	Placements map[string]int `json:"placements"`

	Cost         Cost        `json:"cost"`
	MachineHours float64     `json:"machine_hours"`
	Machines     int         `json:"machines"` // machines up at some point
	Utilisation  Utilisation `json:"utilisation"`
	QueueWait    Wait        `json:"queue_wait"`
	Cache        Cache       `json:"cache"`
	Estimator    Accuracy    `json:"estimator"`
}

// Cost is machine-hours × price. Machine cost is attributed to the tasks
// on a machine by their reserved share of its capacity (mean of the CPU
// and memory shares, at most 1); the rest is idle cost.
type Cost struct {
	Total      float64   `json:"total"`
	Attributed float64   `json:"attributed"`
	Idle       float64   `json:"idle"`
	ByDay      []KeyCost `json:"by_day"`  // UTC days, machine cost
	ByRepo     []KeyCost `json:"by_repo"` // attributed cost
	BySize     []KeyCost `json:"by_size"` // machine cost by provider/size
}

// KeyCost is the cost of one group.
type KeyCost struct {
	Key   string  `json:"key"`
	Cost  float64 `json:"cost"`
	Hours float64 `json:"hours"`           // machine hours (day, size) or session hours (repo)
	Tasks int     `json:"tasks,omitempty"` // repo: finished sessions
}

// Utilisation compares capacity, reservations and measured usage.
// Capacity is core- and GiB-hours of machines up; reserved is the sum of
// session reservations × duration; used is CPU time and peak memory ×
// duration. Overprovisioning is 1 - used/reserved.
type Utilisation struct {
	CapacityCPUHours  float64 `json:"capacity_cpu_hours"`
	CapacityMemGiBH   float64 `json:"capacity_memory_gib_hours"`
	ReservedCPUHours  float64 `json:"reserved_cpu_hours"`
	ReservedMemGiBH   float64 `json:"reserved_memory_gib_hours"`
	UsedCPUHours      float64 `json:"used_cpu_hours"`
	PeakMemGiBH       float64 `json:"peak_memory_gib_hours"`
	ReservedCPU       float64 `json:"reserved_cpu"`    // reserved / capacity
	ReservedMemory    float64 `json:"reserved_memory"` // reserved / capacity
	UsedCPU           float64 `json:"used_cpu"`        // used / capacity
	OverprovisionCPU  float64 `json:"overprovision_cpu"`
	OverprovisionMem  float64 `json:"overprovision_memory"`
	SessionHours      float64 `json:"session_hours"`
	CapacityUnknownMH float64 `json:"capacity_unknown_machine_hours,omitempty"`
}

// Wait is the queue wait of assigned requests.
type Wait struct {
	Count int           `json:"count"`
	P50   time.Duration `json:"p50"`
	P95   time.Duration `json:"p95"`
	Max   time.Duration `json:"max"`
}

// Cache counts build cache reuse and compiler cache counters.
type Cache struct {
	Warm      int              `json:"warm"`
	Cold      int              `json:"cold"`
	WarmRatio float64          `json:"warm_ratio"`
	Counters  map[string]int64 `json:"counters,omitempty"` // summed Usage.Cache (sccache_hits, ...)
}

// Accuracy compares the memory estimate (request) with the measured peak.
type Accuracy struct {
	Samples int `json:"samples"`
	// Under counts sessions whose peak exceeded the estimate; OOM those
	// killed for exceeding their limit.
	Under int `json:"under"`
	OOM   int `json:"oom"`
	// Ratio is peak / estimate (p50, p95, mean): below 1 means
	// overestimated (wasted reservation), above 1 underestimated.
	RatioP50  float64 `json:"ratio_p50"`
	RatioP95  float64 `json:"ratio_p95"`
	RatioMean float64 `json:"ratio_mean"`
	// MAPE is the mean absolute error relative to the peak.
	MAPE float64 `json:"mape"`
	// CPURatio is the mean of busy cores (CPU seconds / wall) / estimate.
	CPURatio float64 `json:"cpu_ratio"`
}

const gib = float64(1 << 30)

type interval struct{ start, end time.Time }

type machineState struct {
	provider, size string
	price          float64
	cpu            float64
	mem            int64
	up             bool
	since          time.Time
	spans          []interval
}

func upEvent(ev events.Event) (up, known bool) {
	switch ev.Type {
	case events.MachineCreate, events.MachineStart, events.MachineReady:
		return true, true
	case events.MachineStop, events.MachineDestroy:
		return false, true
	case events.MachineState:
		switch provider.State(ev.State) {
		case provider.Running, provider.Starting:
			return true, true
		}
		return false, true
	}
	return false, false
}

// clip returns the overlap of [s, e) with [from, to).
func clip(s, e, from, to time.Time) (time.Time, time.Time, bool) {
	if s.Before(from) {
		s = from
	}
	if e.After(to) {
		e = to
	}
	return s, e, e.After(s)
}

// Compute builds the report of [from, to) from events (which should
// start Lookback before from, so that machines already up are counted).
func Compute(evs []events.Event, from, to time.Time) Report {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
	r := Report{From: from, To: to, Placements: map[string]int{}}
	machines := map[string]*machineState{}
	var order []string
	for _, ev := range evs {
		up, known := upEvent(ev)
		if !known || ev.Machine == "" {
			continue
		}
		m, ok := machines[ev.Machine]
		if !ok {
			m = &machineState{}
			machines[ev.Machine] = m
			order = append(order, ev.Machine)
		}
		if ev.Provider != "" {
			m.provider = ev.Provider
		}
		if ev.Size != "" {
			m.size = ev.Size
		}
		if ev.Price > 0 {
			m.price = ev.Price
		}
		if ev.Capacity != nil && ev.Capacity.CPU > 0 {
			m.cpu, m.mem = ev.Capacity.CPU, ev.Capacity.Memory
		}
		switch {
		case up && !m.up:
			m.up, m.since = true, ev.Time
		case !up && m.up:
			m.up = false
			m.spans = append(m.spans, interval{m.since, ev.Time})
		}
	}
	byDay := map[string]*KeyCost{}
	bySize := map[string]*KeyCost{}
	for _, id := range order {
		m := machines[id]
		if m.up {
			m.spans = append(m.spans, interval{m.since, to})
		}
		counted := false
		for _, sp := range m.spans {
			s, e, ok := clip(sp.start, sp.end, from, to)
			if !ok {
				continue
			}
			counted = true
			h := e.Sub(s).Hours()
			r.MachineHours += h
			r.Cost.Total += h * m.price
			if m.cpu > 0 {
				r.Utilisation.CapacityCPUHours += h * m.cpu
				r.Utilisation.CapacityMemGiBH += h * float64(m.mem) / gib
			} else {
				r.Utilisation.CapacityUnknownMH += h
			}
			sk := m.provider + "/" + m.size
			if bySize[sk] == nil {
				bySize[sk] = &KeyCost{Key: sk}
			}
			bySize[sk].Hours += h
			bySize[sk].Cost += h * m.price
			// Split at UTC midnights for the daily cost.
			for d := s; d.Before(e); {
				next := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, time.UTC)
				if next.After(e) {
					next = e
				}
				day := d.UTC().Format("2006-01-02")
				if byDay[day] == nil {
					byDay[day] = &KeyCost{Key: day}
				}
				dh := next.Sub(d).Hours()
				byDay[day].Hours += dh
				byDay[day].Cost += dh * m.price
				d = next
			}
		}
		if counted {
			r.Machines++
		}
	}
	byRepo := map[string]*KeyCost{}
	var waits []time.Duration
	var ratios []float64
	var cpuRatios []float64
	var absErr float64
	for _, ev := range evs {
		switch ev.Type {
		case events.TaskPlaced:
			if ev.Time.Before(from) || !ev.Time.Before(to) {
				continue
			}
			r.Placements[ev.Placement]++
			if ev.Placement == "assign" {
				waits = append(waits, time.Duration(ev.WaitMs)*time.Millisecond)
			}
		case events.TaskEnd:
			u := ev.Usage
			if u == nil || ev.Time.Before(from) || !ev.Time.Before(to) {
				continue
			}
			r.Tasks++
			if u.Warm {
				r.Cache.Warm++
			} else {
				r.Cache.Cold++
			}
			for k, v := range u.Cache {
				if r.Cache.Counters == nil {
					r.Cache.Counters = map[string]int64{}
				}
				r.Cache.Counters[k] += v
			}
			end := ev.Time
			start := end.Add(-u.Wall)
			s, e, ok := clip(start, end, from, to)
			if ok && u.Wall > 0 {
				h := e.Sub(s).Hours()
				frac := e.Sub(s).Seconds() / u.Wall.Seconds()
				r.Utilisation.SessionHours += h
				r.Utilisation.UsedCPUHours += u.CPUSeconds / 3600 * frac
				r.Utilisation.PeakMemGiBH += h * float64(u.PeakMemory) / gib
				share := 1.0
				if res := ev.Reserve; res != nil {
					r.Utilisation.ReservedCPUHours += h * res.CPU
					r.Utilisation.ReservedMemGiBH += h * float64(res.Memory) / gib
					if c := ev.Capacity; c != nil && c.CPU > 0 && c.Memory > 0 {
						share = math.Min(1, (res.CPU/c.CPU+float64(res.Memory)/float64(c.Memory))/2)
					}
				}
				cost := h * ev.Price * share
				r.Cost.Attributed += cost
				repo := ev.Repo
				if repo == "" {
					repo = "(unknown)"
				}
				if byRepo[repo] == nil {
					byRepo[repo] = &KeyCost{Key: repo}
				}
				byRepo[repo].Cost += cost
				byRepo[repo].Hours += h
				byRepo[repo].Tasks++
			}
			if u.OOM {
				r.Estimator.OOM++
			}
			if n := ev.Need; n != nil && n.Memory > 0 && u.PeakMemory > 0 {
				ratio := float64(u.PeakMemory) / float64(n.Memory)
				ratios = append(ratios, ratio)
				absErr += math.Abs(float64(u.PeakMemory)-float64(n.Memory)) / float64(u.PeakMemory)
				if ratio > 1 {
					r.Estimator.Under++
				}
			}
			if n := ev.Need; n != nil && n.CPU > 0 && u.Wall > 0 && u.CPUSeconds > 0 {
				cpuRatios = append(cpuRatios, u.CPUSeconds/u.Wall.Seconds()/n.CPU)
			}
		}
	}
	r.Cost.Idle = math.Max(0, r.Cost.Total-r.Cost.Attributed)
	r.Cost.ByDay = sorted(byDay, false)
	r.Cost.BySize = sorted(bySize, true)
	r.Cost.ByRepo = sorted(byRepo, true)
	ut := &r.Utilisation
	ut.ReservedCPU = div(ut.ReservedCPUHours, ut.CapacityCPUHours)
	ut.ReservedMemory = div(ut.ReservedMemGiBH, ut.CapacityMemGiBH)
	ut.UsedCPU = div(ut.UsedCPUHours, ut.CapacityCPUHours)
	if ut.ReservedCPUHours > 0 {
		ut.OverprovisionCPU = 1 - ut.UsedCPUHours/ut.ReservedCPUHours
	}
	if ut.ReservedMemGiBH > 0 {
		ut.OverprovisionMem = 1 - ut.PeakMemGiBH/ut.ReservedMemGiBH
	}
	r.QueueWait = waitStats(waits)
	r.Cache.WarmRatio = div(float64(r.Cache.Warm), float64(r.Cache.Warm+r.Cache.Cold))
	if n := len(ratios); n > 0 {
		r.Estimator.Samples = n
		sum := 0.0
		for _, x := range ratios {
			sum += x
		}
		sort.Float64s(ratios)
		r.Estimator.RatioMean = sum / float64(n)
		r.Estimator.RatioP50 = percentile(ratios, 0.5)
		r.Estimator.RatioP95 = percentile(ratios, 0.95)
		r.Estimator.MAPE = absErr / float64(n)
	}
	if n := len(cpuRatios); n > 0 {
		sum := 0.0
		for _, x := range cpuRatios {
			sum += x
		}
		r.Estimator.CPURatio = sum / float64(n)
	}
	return r
}

func div(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// percentile of sorted values (nearest rank).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[min(i, len(sorted)-1)]
}

func waitStats(ws []time.Duration) Wait {
	if len(ws) == 0 {
		return Wait{}
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
	f := make([]float64, len(ws))
	for i, w := range ws {
		f[i] = float64(w)
	}
	return Wait{Count: len(ws), P50: time.Duration(percentile(f, 0.5)), P95: time.Duration(percentile(f, 0.95)), Max: ws[len(ws)-1]}
}

// sorted returns groups by key, or by cost (descending) when byCost.
func sorted(m map[string]*KeyCost, byCost bool) []KeyCost {
	out := make([]KeyCost, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if byCost && out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Write prints the report for humans.
func (r Report) Write(w io.Writer) {
	fmt.Fprintf(w, "report %s .. %s (UTC)\n", r.From.UTC().Format("2006-01-02 15:04"), r.To.UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "\ncost: $%.2f total, $%.2f attributed to sessions, $%.2f idle (%.1f machine-hours on %d machines)\n",
		r.Cost.Total, r.Cost.Attributed, r.Cost.Idle, r.MachineHours, r.Machines)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	section := func(title string, rows []KeyCost, hoursLabel string) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(tw, "\n%s\tcost $\t%s\t\n", title, hoursLabel)
		for _, k := range rows {
			extra := ""
			if k.Tasks > 0 {
				extra = fmt.Sprintf("%d sessions", k.Tasks)
			}
			fmt.Fprintf(tw, "  %s\t%.2f\t%.2f\t%s\n", k.Key, k.Cost, k.Hours, extra)
		}
	}
	section("by day", r.Cost.ByDay, "machine-h")
	section("by size", r.Cost.BySize, "machine-h")
	section("by repository", r.Cost.ByRepo, "session-h")
	tw.Flush()
	u := r.Utilisation
	fmt.Fprintf(w, "\nutilisation: reserved %.0f%% CPU, %.0f%% memory of capacity; used %.0f%% CPU of capacity\n",
		100*u.ReservedCPU, 100*u.ReservedMemory, 100*u.UsedCPU)
	fmt.Fprintf(w, "overprovisioning: %.0f%% of reserved CPU and %.0f%% of reserved memory unused (%.1f session-hours)\n",
		100*u.OverprovisionCPU, 100*u.OverprovisionMem, u.SessionHours)
	var pl []string
	for k, n := range r.Placements {
		pl = append(pl, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(pl)
	fmt.Fprintf(w, "\nrequests: %s; %d sessions finished\n", strings.Join(pl, ", "), r.Tasks)
	q := r.QueueWait
	fmt.Fprintf(w, "queue wait: p50 %s, p95 %s, max %s (%d assigned)\n", q.P50.Round(time.Millisecond), q.P95.Round(time.Millisecond), q.Max.Round(time.Millisecond), q.Count)
	c := r.Cache
	fmt.Fprintf(w, "build cache: %d warm, %d cold (%.0f%% warm)", c.Warm, c.Cold, 100*c.WarmRatio)
	if h, ok := c.Counters["sccache_hits"]; ok {
		fmt.Fprintf(w, "; sccache %d hits / %d misses", h, c.Counters["sccache_misses"])
	}
	fmt.Fprintln(w)
	e := r.Estimator
	fmt.Fprintf(w, "estimator: peak/estimate memory p50 %.2f, p95 %.2f, mean %.2f, MAPE %.0f%%; %d of %d above estimate, %d OOM; busy cores/estimate %.2f\n",
		e.RatioP50, e.RatioP95, e.RatioMean, 100*e.MAPE, e.Under, e.Samples, e.OOM, e.CPURatio)
}
