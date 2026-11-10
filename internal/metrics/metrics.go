// Package metrics is a small Prometheus-compatible metrics registry:
// counters, gauges and histograms with labels, snapshots that travel as
// JSON (machine servers report theirs in the status stream) and the text
// exposition format (version 0.0.4) for scraping.
package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Metric types.
const (
	Counter   = "counter"
	Gauge     = "gauge"
	Histogram = "histogram"
)

// Label is one label pair of a sample.
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Bucket is a cumulative histogram bucket. The +Inf bucket is implied by
// Sample.Count (JSON cannot encode infinity).
type Bucket struct {
	UpperBound float64 `json:"le"`
	Count      uint64  `json:"count"`
}

// Sample is one time series of a family.
type Sample struct {
	Labels  []Label  `json:"labels,omitempty"`
	Value   float64  `json:"value,omitempty"`   // counter, gauge
	Buckets []Bucket `json:"buckets,omitempty"` // histogram
	Count   uint64   `json:"count,omitempty"`   // histogram
	Sum     float64  `json:"sum,omitempty"`     // histogram
}

// Family is a named metric with its samples.
type Family struct {
	Name    string   `json:"name"`
	Help    string   `json:"help,omitempty"`
	Type    string   `json:"type"`
	Samples []Sample `json:"samples,omitempty"`
}

// DurationBuckets suit waits and run times from sub-second to an hour.
var DurationBuckets = []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}

var nameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Registry holds metric vectors and collectors. The zero value is not
// usable; use NewRegistry.
type Registry struct {
	mu         sync.Mutex
	vecs       map[string]*vec
	collectors []func() []Family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{vecs: map[string]*vec{}} }

type vec struct {
	name, help, kind string
	labels           []string
	buckets          []float64

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	value  float64
	counts []uint64 // per bucket, not cumulative
	count  uint64
	sum    float64
}

func (r *Registry) register(name, help, kind string, buckets []float64, labels []string) *vec {
	if !nameRe.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid name %q", name))
	}
	for _, l := range labels {
		if !nameRe.MatchString(l) || l == "le" {
			panic(fmt.Sprintf("metrics: invalid label %q of %s", l, name))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.vecs[name]; ok {
		if v.kind != kind || strings.Join(v.labels, ",") != strings.Join(labels, ",") {
			panic(fmt.Sprintf("metrics: %s registered twice with different shape", name))
		}
		return v
	}
	v := &vec{name: name, help: help, kind: kind, labels: labels, series: map[string]*series{}}
	if kind == Histogram {
		v.buckets = append([]float64(nil), buckets...)
		sort.Float64s(v.buckets)
	}
	r.vecs[name] = v
	return v
}

func (v *vec) get(values []string) *series {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s, ok := v.series[key]
	if !ok {
		s = &series{values: append([]string(nil), values...)}
		if v.kind == Histogram {
			s.counts = make([]uint64, len(v.buckets))
		}
		v.series[key] = s
	}
	return s
}

// CounterVec is a counter with labels.
type CounterVec struct{ v *vec }

// GaugeVec is a gauge with labels.
type GaugeVec struct{ v *vec }

// HistogramVec is a histogram with labels.
type HistogramVec struct{ v *vec }

// Counter registers (or returns the existing) counter vector.
func (r *Registry) Counter(name, help string, labels ...string) CounterVec {
	return CounterVec{r.register(name, help, Counter, nil, labels)}
}

// Gauge registers (or returns the existing) gauge vector.
func (r *Registry) Gauge(name, help string, labels ...string) GaugeVec {
	return GaugeVec{r.register(name, help, Gauge, nil, labels)}
}

// Histogram registers (or returns the existing) histogram vector.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) HistogramVec {
	return HistogramVec{r.register(name, help, Histogram, buckets, labels)}
}

// Collect adds a function that produces families at gather time (e.g.
// gauges derived from current state). It must not use this registry.
func (r *Registry) Collect(fn func() []Family) {
	r.mu.Lock()
	r.collectors = append(r.collectors, fn)
	r.mu.Unlock()
}

// Add adds d (must be >= 0) to the series with the given label values.
func (c CounterVec) Add(d float64, values ...string) {
	if d < 0 {
		return
	}
	c.v.mu.Lock()
	c.v.get(values).value += d
	c.v.mu.Unlock()
}

// Inc adds one.
func (c CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Set sets the series.
func (g GaugeVec) Set(x float64, values ...string) {
	g.v.mu.Lock()
	g.v.get(values).value = x
	g.v.mu.Unlock()
}

// Add adds d to the series.
func (g GaugeVec) Add(d float64, values ...string) {
	g.v.mu.Lock()
	g.v.get(values).value += d
	g.v.mu.Unlock()
}

// Observe records one value.
func (h HistogramVec) Observe(x float64, values ...string) {
	h.v.mu.Lock()
	s := h.v.get(values)
	for i, b := range h.v.buckets {
		if x <= b {
			s.counts[i]++
			break
		}
	}
	s.count++
	s.sum += x
	h.v.mu.Unlock()
}

func (v *vec) family() Family {
	v.mu.Lock()
	defer v.mu.Unlock()
	f := Family{Name: v.name, Help: v.help, Type: v.kind}
	for _, s := range v.series {
		smp := Sample{}
		for i, l := range v.labels {
			smp.Labels = append(smp.Labels, Label{l, s.values[i]})
		}
		if v.kind == Histogram {
			var cum uint64
			for i, b := range v.buckets {
				cum += s.counts[i]
				smp.Buckets = append(smp.Buckets, Bucket{b, cum})
			}
			smp.Count, smp.Sum = s.count, s.sum
		} else {
			smp.Value = s.value
		}
		f.Samples = append(f.Samples, smp)
	}
	return f
}

// Gather returns all families sorted by name; samples are sorted by labels.
// Vectors without series are included (with HELP and TYPE only).
func (r *Registry) Gather() []Family {
	r.mu.Lock()
	vecs := make([]*vec, 0, len(r.vecs))
	for _, v := range r.vecs {
		vecs = append(vecs, v)
	}
	collectors := append([]func() []Family(nil), r.collectors...)
	r.mu.Unlock()
	var sets [][]Family
	var own []Family
	for _, v := range vecs {
		own = append(own, v.family())
	}
	sets = append(sets, own)
	for _, c := range collectors {
		sets = append(sets, c())
	}
	return Merge(sets...)
}

// Merge combines family sets: samples of families with the same name are
// concatenated (type and help of the first occurrence win; families of a
// conflicting type are dropped). The result is sorted.
func Merge(sets ...[]Family) []Family {
	byName := map[string]*Family{}
	var names []string
	for _, set := range sets {
		for _, f := range set {
			if !nameRe.MatchString(f.Name) {
				continue
			}
			g, ok := byName[f.Name]
			if !ok {
				c := Family{Name: f.Name, Help: f.Help, Type: f.Type}
				byName[f.Name] = &c
				names = append(names, f.Name)
				g = &c
			} else if g.Type != f.Type {
				continue
			}
			g.Samples = append(g.Samples, f.Samples...)
		}
	}
	sort.Strings(names)
	out := make([]Family, 0, len(names))
	for _, n := range names {
		f := *byName[n]
		sort.SliceStable(f.Samples, func(i, j int) bool { return labelKey(f.Samples[i].Labels) < labelKey(f.Samples[j].Labels) })
		out = append(out, f)
	}
	return out
}

func labelKey(ls []Label) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Name)
		b.WriteByte(0)
		b.WriteString(l.Value)
		b.WriteByte(0)
	}
	return b.String()
}

// WithLabels returns a copy of fams with labels prepended to every sample.
// A sample's own label of the same name is replaced.
func WithLabels(fams []Family, labels ...Label) []Family {
	out := make([]Family, 0, len(fams))
	for _, f := range fams {
		c := f
		c.Samples = make([]Sample, 0, len(f.Samples))
		for _, s := range f.Samples {
			n := s
			n.Labels = append([]Label(nil), labels...)
			for _, l := range s.Labels {
				dup := false
				for _, x := range labels {
					dup = dup || x.Name == l.Name
				}
				if !dup {
					n.Labels = append(n.Labels, l)
				}
			}
			c.Samples = append(c.Samples, n)
		}
		out = append(out, c)
	}
	return out
}

// GaugeFamily builds a gauge family for collectors.
func GaugeFamily(name, help string, samples ...Sample) Family {
	return Family{Name: name, Help: help, Type: Gauge, Samples: samples}
}

// L builds labels from name/value pairs.
func L(pairs ...string) []Label {
	var out []Label
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Label{pairs[i], pairs[i+1]})
	}
	return out
}
