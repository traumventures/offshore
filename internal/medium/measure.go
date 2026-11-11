package medium

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Measured profiles. Workers count the transfers of their tiers (page-ins
// = reads from a lower medium, writebacks and page-outs = writes) in
// cumulative Counters per medium (Recorder) and report them in their
// status. The orchestrator turns the deltas per machine into decayed sums
// per provider and medium (Meter) and fits seconds = latency +
// bytes/throughput over the requests; Meter.Profile is the Measured
// source installed with SetMeasured.

// Fit holds the sums of a least-squares fit of request time over request
// size (x = bytes, y = seconds).
type Fit struct {
	N   float64 `json:"n"`
	X   float64 `json:"x"`   // Σ bytes
	Y   float64 `json:"y"`   // Σ seconds
	XX  float64 `json:"xx"`  // Σ bytes²
	XY  float64 `json:"xy"`  // Σ bytes·seconds
	Max float64 `json:"max"` // largest request (bytes), not summed
}

// Add adds one request.
func (f *Fit) Add(bytes int64, seconds float64) {
	x := float64(bytes)
	f.N++
	f.X += x
	f.Y += seconds
	f.XX += x * x
	f.XY += x * seconds
	f.Max = max(f.Max, x)
}

func (f Fit) sub(o Fit) Fit {
	return Fit{N: f.N - o.N, X: f.X - o.X, Y: f.Y - o.Y, XX: f.XX - o.XX, XY: f.XY - o.XY, Max: f.Max}
}

func (f Fit) scale(k float64) Fit {
	return Fit{N: f.N * k, X: f.X * k, Y: f.Y * k, XX: f.XX * k, XY: f.XY * k, Max: f.Max}
}

func (f Fit) plus(o Fit) Fit {
	return Fit{N: f.N + o.N, X: f.X + o.X, Y: f.Y + o.Y, XX: f.XX + o.XX, XY: f.XY + o.XY, Max: max(f.Max, o.Max)}
}

// Estimate returns the throughput (bytes/s) and the per request latency
// of the fit. With too little spread in the request sizes the latency is
// unknown (0) and the throughput is the plain ratio of bytes to seconds.
func (f Fit) Estimate() (throughput int64, latency time.Duration) {
	if f.N <= 0 || f.Y <= 0 || f.X <= 0 {
		return 0, 0
	}
	ratio := int64(f.X / f.Y)
	vx := f.XX/f.N - (f.X/f.N)*(f.X/f.N) // variance of the sizes
	mean := f.X / f.N
	if vx <= 0 || math.Sqrt(vx) < 0.25*mean {
		return ratio, 0
	}
	slope := (f.XY/f.N - mean*(f.Y/f.N)) / vx
	icept := f.Y/f.N - slope*mean
	if slope <= 0 || icept < 0 {
		return ratio, 0
	}
	return int64(1 / slope), time.Duration(icept * float64(time.Second))
}

// Counters are the cumulative transfers of one medium on a worker.
type Counters struct {
	Medium string `json:"medium"`
	Reads  Fit    `json:"reads"`  // page-ins from the medium
	Writes Fit    `json:"writes"` // writebacks and page-outs to it
}

// MediumOf maps a tier name ("store", "volume>store") to the medium it
// transfers from first ("" when the name is not a medium).
func MediumOf(tier string) string {
	m, _, _ := strings.Cut(tier, ">")
	switch m {
	case Local, Volume, Store:
		return m
	}
	return ""
}

// Recorder collects Counters on a worker. Safe for concurrent use; the
// zero value is ready.
type Recorder struct {
	mu sync.Mutex
	c  map[string]*Counters
}

// Add records one transfer of bytes in seconds (write: to the medium).
func (r *Recorder) Add(med string, write bool, bytes int64, seconds float64) {
	if med == "" || bytes <= 0 || seconds <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c == nil {
		r.c = map[string]*Counters{}
	}
	c := r.c[med]
	if c == nil {
		c = &Counters{Medium: med}
		r.c[med] = c
	}
	if write {
		c.Writes.Add(bytes, seconds)
	} else {
		c.Reads.Add(bytes, seconds)
	}
}

// Snapshot returns the counters sorted by medium (nil: none).
func (r *Recorder) Snapshot() []Counters {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Counters
	for _, c := range r.c {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Medium < out[j].Medium })
	return out
}

// Meter aggregates worker counters per provider instance and medium into
// exponentially decayed fits (time constant Tau, default one hour).
// Safe for concurrent use; use NewMeter.
type Meter struct {
	mu   sync.Mutex
	last map[string]map[string]Counters // machine -> medium -> cumulative
	agg  map[[2]string]*agg             // provider, medium
	// Tau is the decay time constant; MinRequests the decayed request
	// count a fit needs before it is reported.
	Tau         time.Duration
	MinRequests float64
	now         func() time.Time
}

type agg struct {
	reads, writes Fit
	at            time.Time
}

// NewMeter returns an empty meter.
func NewMeter() *Meter {
	return &Meter{last: map[string]map[string]Counters{}, agg: map[[2]string]*agg{}, Tau: time.Hour, MinRequests: 16, now: time.Now}
}

// Update folds a machine's cumulative counters (from its status) into
// its provider's aggregates. Counters that went backwards (a restarted
// worker) count from zero.
func (m *Meter) Update(machine, provider string, cs []Counters) {
	if provider == "" || len(cs) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	prev := m.last[machine]
	next := make(map[string]Counters, len(cs))
	for _, c := range cs {
		next[c.Medium] = c
		p, ok := prev[c.Medium]
		dr, dw := c.Reads, c.Writes
		if ok {
			if c.Reads.N >= p.Reads.N && c.Writes.N >= p.Writes.N {
				dr, dw = c.Reads.sub(p.Reads), c.Writes.sub(p.Writes)
			}
		}
		if dr.N <= 0 && dw.N <= 0 {
			continue
		}
		k := [2]string{provider, c.Medium}
		a := m.agg[k]
		if a == nil {
			a = &agg{at: now}
			m.agg[k] = a
		}
		decay := math.Exp(-now.Sub(a.at).Seconds() / m.Tau.Seconds())
		a.reads = a.reads.scale(decay).plus(clean(dr))
		a.writes = a.writes.scale(decay).plus(clean(dw))
		a.at = now
	}
	m.last[machine] = next
}

// clean drops a delta without requests.
func clean(f Fit) Fit {
	if f.N <= 0 {
		return Fit{}
	}
	return f
}

// Forget drops a machine's last counters (it is gone).
func (m *Meter) Forget(machine string) {
	m.mu.Lock()
	delete(m.last, machine)
	m.mu.Unlock()
}

// Profile is the measured profile of a provider instance's medium: the
// read fit's throughput (else the write fit's) and latency. It is a
// Measured source for SetMeasured.
func (m *Meter) Profile(provider, med string) (Profile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.agg[[2]string{provider, med}]
	if a == nil {
		return Profile{}, false
	}
	p := Profile{Medium: med}
	if a.reads.N >= m.MinRequests {
		p.Throughput, p.Latency = a.reads.Estimate()
	}
	if p.Throughput == 0 && a.writes.N >= m.MinRequests {
		p.Throughput, _ = a.writes.Estimate()
	}
	if p.Throughput == 0 && p.Latency == 0 {
		return Profile{}, false
	}
	return p, true
}
