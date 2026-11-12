package sim

import (
	"math"
	"math/rand/v2"
	"time"
)

const gib = int64(1) << 30

// Repo is the build profile of one repository in a synthetic trace.
type Repo struct {
	Name     string
	Env      string  // repositories sharing an environment can share machines
	Rate     float64 // builds per hour at peak working hours
	Duration float64 // mean warm build duration in seconds (log-normal)
	CPU      float64
	Memory   int64   // configured need
	Peak     float64 // mean peak memory as a fraction of Memory
	Burst    float64 // probability that a build starts a burst of 2-5 follow-ups
}

// GenOptions configures Generate.
type GenOptions struct {
	Seed     uint64
	Start    time.Time // first day, midnight in Location (default DefaultStart)
	Days     int       // default 5
	Location *time.Location
	Repos    []Repo // default Profiles["team"]
}

// Profiles are built-in repository mixes for synthetic traces.
var Profiles = map[string][]Repo{
	// small: one developer group, two environments.
	"small": {
		{Name: "api", Env: "rust", Rate: 5, Duration: 180, CPU: 4, Memory: 4 * gib, Peak: 0.6, Burst: 0.2},
		{Name: "cli", Env: "rust", Rate: 3, Duration: 90, CPU: 2, Memory: 2 * gib, Peak: 0.5, Burst: 0.1},
		{Name: "web", Env: "node", Rate: 6, Duration: 100, CPU: 2, Memory: 3 * gib, Peak: 0.7, Burst: 0.2},
	},
	// solo: one developer with one repository, a few builds an hour
	// (the 0-1 machine regime).
	"solo": {
		{Name: "app", Env: "rust", Rate: 3, Duration: 150, CPU: 4, Memory: 6 * gib, Peak: 0.6, Burst: 0.3},
	},
	// sparse: one developer, two repositories of one environment, a
	// build every hour or two.
	"sparse": {
		{Name: "app", Env: "rust", Rate: 0.6, Duration: 200, CPU: 4, Memory: 6 * gib, Peak: 0.6, Burst: 0.2},
		{Name: "lib", Env: "rust", Rate: 0.3, Duration: 90, CPU: 2, Memory: 3 * gib, Peak: 0.5, Burst: 0.1},
	},
	// team: a larger team, three environments, one heavy job type.
	"team": {
		{Name: "api", Env: "rust", Rate: 10, Duration: 240, CPU: 4, Memory: 4 * gib, Peak: 0.6, Burst: 0.25},
		{Name: "worker", Env: "rust", Rate: 6, Duration: 180, CPU: 4, Memory: 4 * gib, Peak: 0.55, Burst: 0.2},
		{Name: "cli", Env: "rust", Rate: 5, Duration: 90, CPU: 2, Memory: 2 * gib, Peak: 0.5, Burst: 0.1},
		{Name: "web", Env: "node", Rate: 12, Duration: 120, CPU: 2, Memory: 3 * gib, Peak: 0.7, Burst: 0.3},
		{Name: "docs", Env: "node", Rate: 3, Duration: 45, CPU: 1, Memory: 1 * gib, Peak: 0.5, Burst: 0.1},
		{Name: "ml", Env: "py", Rate: 1, Duration: 900, CPU: 8, Memory: 12 * gib, Peak: 0.8, Burst: 0.05},
	},
}

// intensity is the relative build activity at a local time: working days
// 08:00-19:00 with a morning ramp, a lunch dip and an afternoon peak; a
// trickle of automated builds at night and on weekends.
func intensity(t time.Time) float64 {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return 0.03
	}
	h := float64(t.Hour()) + float64(t.Minute())/60
	switch {
	case h < 8 || h >= 19:
		return 0.03
	case h < 10:
		return 0.3 + 0.35*(h-8)
	case h < 12:
		return 1
	case h < 13:
		return 0.45
	case h < 14:
		return 0.8
	case h < 17:
		return 1.25
	default:
		return 1 - 0.4*(h-17)
	}
}

func poisson(r *rand.Rand, lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// Generate creates a bursty workday trace. It is deterministic for a seed.
func Generate(o GenOptions) Trace {
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Start.IsZero() {
		o.Start = DefaultStart
	}
	start := o.Start.In(o.Location)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, o.Location)
	if o.Days <= 0 {
		o.Days = 5
	}
	if o.Repos == nil {
		o.Repos = Profiles["team"]
	}
	r := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15))
	tr := Trace{Start: start, End: float64(o.Days) * 86400}
	job := func(rp Repo, at float64) Job {
		d := rp.Duration * math.Exp(0.5*r.NormFloat64()-0.125)
		peak := math.Min(math.Max(rp.Peak+0.15*r.NormFloat64(), 0.1), 1.4)
		return Job{At: math.Round(at*10) / 10, Repo: rp.Name, Env: rp.Env, Duration: math.Round(math.Max(d, 5)),
			CPU: rp.CPU, Memory: Bytes(rp.Memory), PeakMemory: Bytes(int64(peak*float64(rp.Memory)) &^ (1<<20 - 1))}
	}
	for minute := 0; minute < o.Days*24*60; minute++ {
		at := float64(minute * 60)
		lvl := intensity(start.Add(time.Duration(minute) * time.Minute))
		for _, rp := range o.Repos {
			for n := poisson(r, rp.Rate*lvl/60); n > 0; n-- {
				t := at + 60*r.Float64()
				tr.Jobs = append(tr.Jobs, job(rp, t))
				if r.Float64() < rp.Burst {
					for k := 2 + r.IntN(4); k > 0; k-- {
						t += 20 + 120*r.Float64()
						tr.Jobs = append(tr.Jobs, job(rp, t))
					}
				}
			}
		}
	}
	tr.sort()
	return tr
}
