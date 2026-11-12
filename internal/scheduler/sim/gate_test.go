package sim

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gateCases are the evaluation cases plus the low-load traces: the
// production days of 2026-11-11 and -12 and synthetic single-developer
// weeks (solo: a few builds an hour, sparse: one every hour or two).
func gateCases(t testing.TB) []evalCase {
	pools := map[string]EvalPool{}
	for _, p := range EvalPools() {
		pools[p.Name] = p
	}
	cs := evalCases(t)
	// Production runs the history estimator: the static one never learns
	// from OOM kills, so with the exported traces (a third of the builds
	// peak above their request) the size choice alone decides how many
	// builds are OOM killed.
	for i := range cs {
		cs[i].pool.History = true
	}
	c7a := pools["c7a"]
	c7a.History = true
	add := func(name string, tr Trace) {
		cs = append(cs, evalCase{name: name + "/c7a", pool: c7a, trace: tr},
			evalCase{name: name + "/prod", pool: pools["prod"], trace: withSlots(tr)})
	}
	for _, name := range []string{"prod-nov11-day", "prod-nov12"} {
		tr, err := Load(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		add(name, tr)
	}
	add("solo", Generate(GenOptions{Seed: 1, Days: 5, Repos: Profiles["solo"]}))
	add("sparse", Generate(GenOptions{Seed: 2, Days: 5, Repos: Profiles["sparse"]}))
	return cs
}

// TestGate compares the scheduler before and after the v2 rewrite on
// every gate case at cost_optimization 0.2 and 0.8 (OFFSHORE_GATE=old,new
// variants, default "default,v2"; OFFSHORE_GATE_CASE filters cases) and
// prints the table. Low-load cases are those where the first variant
// never ran more than two machines.
func TestGate(t *testing.T) {
	vs := os.Getenv("OFFSHORE_GATE")
	if vs == "" {
		t.Skip("set OFFSHORE_GATE=old,new (e.g. default,v2) to run the gate")
	}
	variants := strings.Split(vs, ",")
	rs := []float64{0.2, 0.8}
	if v := os.Getenv("OFFSHORE_GATE_R"); v != "" {
		rs = nil
		for _, x := range strings.Split(v, ",") {
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				t.Fatal(err)
			}
			rs = append(rs, f)
		}
	}
	filter := os.Getenv("OFFSHORE_GATE_CASE")
	var cases []evalCase
	for _, c := range gateCases(t) {
		if filter == "" || strings.Contains(c.name, filter) {
			cases = append(cases, c)
		}
	}
	var specs []Spec
	for _, c := range cases {
		for _, r := range rs {
			for _, v := range variants {
				specs = append(specs, c.pool.Spec(fmt.Sprintf("%s r=%.1f", v, r), c.trace, v, r, c.down, c.resize))
			}
		}
	}
	start := time.Now()
	all := RunAll(specs, 0)
	nv := len(variants)
	type agg struct {
		cost, builds, p95, first, obj float64
		n, firstN                     int
	}
	sums := map[string]*agg{}
	var b strings.Builder
	fmt.Fprintln(&b, "| case | r | policy | cost $ | $/build | turn p95 | wait p95 | first n | first wait p95 | failed | machines | max run |")
	fmt.Fprintln(&b, "|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	i := 0
	for _, c := range cases {
		for _, r := range rs {
			reps := all[i : i+nv]
			i += nv
			load := "high"
			if reps[0].MaxRunning <= 2 {
				load = "low"
			}
			for k, rep := range reps {
				per := rep.Cost / math.Max(float64(rep.Completed), 1)
				fmt.Fprintf(&b, "| %s | %.1f | %s | %.2f | %.3f | %s | %s | %d | %s | %d | %d | %d |\n", c.name, r, variants[k], rep.Cost, per,
					rep.TurnP95.Round(time.Second), rep.WaitP95.Round(time.Second), rep.First, rep.FirstWaitP95.Round(time.Second), rep.Failed, rep.Machines, rep.MaxRunning)
				key := fmt.Sprintf("%s r=%.1f %s", load, r, variants[k])
				a := sums[key]
				if a == nil {
					a = &agg{}
					sums[key] = a
				}
				a.cost += rep.Cost
				// The objective at r: cost + time_value x turnaround hours,
				// time_value = cheapest size price x (1-r)/r.
				pmin := math.Inf(1)
				for _, sz := range c.pool.Sizes {
					if sz.PricePerHour > 0 && !c.pool.Cfg.DisabledProviders[sz.Provider] {
						pmin = math.Min(pmin, sz.PricePerHour)
					}
				}
				a.obj += rep.Cost + pmin*(1-r)/r*rep.TurnHours
				a.builds += float64(rep.Completed)
				// p95 relative to the first variant (geometric mean of ratios).
				a.p95 += math.Log(math.Max(rep.TurnP95.Seconds(), 1) / math.Max(reps[0].TurnP95.Seconds(), 1))
				a.n++
				if rep.First > 0 {
					a.first += rep.FirstWaitP95.Seconds()
					a.firstN++
				}
			}
		}
	}
	fmt.Fprintln(&b, "\n| group | policy | cost $ | $/build | turn p95 vs first (geo) | first wait p95 (mean) | objective $ |")
	fmt.Fprintln(&b, "|---|---|---:|---:|---:|---:|---:|")
	for _, load := range []string{"high", "low"} {
		for _, r := range rs {
			for _, v := range variants {
				a := sums[fmt.Sprintf("%s r=%.1f %s", load, r, v)]
				if a == nil {
					continue
				}
				fmt.Fprintf(&b, "| %s r=%.1f | %s | %.2f | %.3f | %.3f | %.0fs | %.0f |\n", load, r, v, a.cost, a.cost/math.Max(a.builds, 1),
					math.Exp(a.p95/float64(a.n)), a.first/math.Max(float64(a.firstN), 1), a.obj)
			}
		}
	}
	if out := os.Getenv("OFFSHORE_GATE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d simulations in %s\n%s", len(specs), time.Since(start).Round(time.Second), b.String())
}
