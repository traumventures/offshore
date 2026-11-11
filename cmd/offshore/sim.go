package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/pool"
	"github.com/traumventures/offshore/internal/report"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/scheduler/sim"
)

const simUsage = `Usage:
  offshore sim [flags] <source>...
      replay traces against the pool configuration and compare policies;
      a source is a trace (.json, .csv), an event log (events-DAY.jsonl,
      converted like sim export), gen:team or gen:small (a synthetic trace
      per -seeds), scenarios or scenario:NAME (the adversarial scenarios
      per -seeds). Runs go in parallel (-parallel); -table prints one
      compact row per run plus means over seeds, e.g.
      offshore sim -seeds 1-8 -policy default,classic -table gen:team scenarios
  offshore sim -sweep <trace>...
      sweep [scheduler] cost_optimization from 0 (fastest) to 1 (cheapest)
      and print the cost/time frontier of the default policy
  offshore sim -generate out.json [-profile team|small] [-seed N] [-days N] [-tz ZONE]
      write a synthetic bursty workday trace
  offshore sim export [-since 7d] [-until T] [-o trace.json] [-cold-penalty 0.5] [-request-cpu]
      convert the pool's event log (real runs) into a trace

Flags:
`

func simCmd(args []string) error {
	if len(args) > 0 && args[0] == "export" {
		return simExport(args[1:])
	}
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), simUsage); fs.PrintDefaults() }
	cfgPath := fs.String("config", "", "pool configuration (TOML, [pool] section); default: the active pool configuration")
	policies := fs.String("policy", "default,binpack,classic", "comma separated policies to compare (default, costmodel, binpack, classic)")
	costOpt := fs.String("cost-optimization", "", "comma separated cost_optimization values for the default policy (default: the pool's)")
	sweep := fs.Bool("sweep", false, "sweep cost_optimization 0..1 in steps of 0.1 and print the cost/time frontier")
	resize := fs.Bool("resize", false, "machines can grow their disks (slot handling)")
	refetch := fs.Float64("refetch", 0, "true refetch factor of the simulated cache file systems: refaulted bytes per byte of working set beyond the local disk (0 = the scheduler default); the policy calibrates it from the simulated events")
	refetchSpread := fs.Float64("refetch-spread", 0, "spread of the true refetch factor across repositories: each repository's factor is refetch * F^u with u in [-1,1] from its name (0 = the same factor everywhere); the policy learns it per key")
	wsFlag := fs.String("working-set", "", "working set of jobs that have none (e.g. 30G; stresses the tier stacks on a trace without cache data)")
	slotFlag := fs.String("slot-bytes", "", "slot size after a cold build of jobs that have none (e.g. 20G; growth per warm build 5%)")
	estimator := fs.String("estimator", "", "static | history (default: pool.estimator)")
	boot := fs.String("boot", "60s", "create latency until a machine is ready: DUR or provider=DUR,...")
	start := fs.String("start", "20s", "start latency of a stopped machine: DUR or provider=DUR,...")
	cold := fs.Float64("cold-penalty", 0.5, "extra build time without a warm cache, as a fraction (-1 = none)")
	timeout := fs.Duration("queue-timeout", 0, "fail tasks waiting longer (0 = wait forever)")
	asJSON := fs.Bool("json", false, "print reports as JSON")
	gen := fs.String("generate", "", "write a synthetic trace to this file instead of simulating")
	profile := fs.String("profile", "team", "synthetic repository mix: team | small")
	seed := fs.Uint64("seed", 1, "random seed of the synthetic trace")
	days := fs.Int("days", 5, "days of the synthetic trace (starting Monday 2026-11-09)")
	tz := fs.String("tz", "UTC", "time zone of the synthetic working hours")
	seedsFlag := fs.String("seeds", "", "seeds of the synthetic sources gen:PROFILE, scenarios and scenario:NAME: 1,2,5 or 1-8 (default: -seed)")
	parallel := fs.Int("parallel", sim.DefaultParallel(), "simulations to run concurrently (default: CPUs - 1)")
	table := fs.Bool("table", false, "print one compact row per run (and the mean over seeds per trace and policy)")
	noCache := fs.Bool("no-cache", false, "parse trace files and event logs again instead of using the binary cache")
	cpuProfile := fs.String("cpuprofile", "", "write a CPU profile of the simulation to this file")
	memProfile := fs.String("memprofile", "", "write an allocation profile of the simulation to this file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	if *memProfile != "" {
		defer func() {
			if f, err := os.Create(*memProfile); err == nil {
				pprof.Lookup("allocs").WriteTo(f, 0)
				f.Close()
			}
		}()
	}
	if *gen != "" {
		repos, ok := sim.Profiles[*profile]
		if !ok {
			return fmt.Errorf("unknown profile %q (team, small)", *profile)
		}
		loc, err := time.LoadLocation(*tz)
		if err != nil {
			return err
		}
		tr := sim.Generate(sim.GenOptions{Seed: *seed, Days: *days, Location: loc, Repos: repos,
			Start: time.Date(2026, 11, 9, 0, 0, 0, 0, loc)})
		if err := tr.Save(*gen); err != nil {
			return err
		}
		fmt.Printf("wrote %d jobs to %s\n", len(tr.Jobs), *gen)
		return nil
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("sim: no trace given")
	}
	if *cfgPath == "" {
		*cfgPath = config.PoolConfigPath()
	}
	u, err := config.LoadPool(*cfgPath)
	if err != nil {
		return err
	}
	oc := pool.Config(u)
	bootLat, err := latencies(*boot)
	if err != nil {
		return fmt.Errorf("-boot: %w", err)
	}
	startLat, err := latencies(*start)
	if err != nil {
		return fmt.Errorf("-start: %w", err)
	}
	var ws, slot int64
	if *wsFlag != "" {
		if ws, err = config.ParseBytes(*wsFlag); err != nil {
			return fmt.Errorf("-working-set: %w", err)
		}
	}
	if *slotFlag != "" {
		if slot, err = config.ParseBytes(*slotFlag); err != nil {
			return fmt.Errorf("-slot-bytes: %w", err)
		}
	}
	est := u.Pool.Estimator
	if *estimator != "" {
		est = *estimator
	}
	type variant struct {
		name string
		r    float64
	}
	var variants []variant
	ratios := []float64{oc.Policy.CostOptimization}
	if *costOpt != "" {
		ratios = nil
		for _, v := range strings.Split(*costOpt, ",") {
			r, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || r < 0 || r > 1 {
				return fmt.Errorf("-cost-optimization: %q is not between 0 and 1", v)
			}
			ratios = append(ratios, r)
		}
	}
	if *sweep {
		ratios = nil
		for i := 0; i <= 10; i++ {
			ratios = append(ratios, float64(i)/10)
		}
		*policies = scheduler.VariantDefault
	}
	for _, name := range strings.Split(*policies, ",") {
		name = strings.TrimSpace(name)
		switch name {
		case scheduler.VariantDefault, scheduler.VariantCostModel:
			for _, r := range ratios {
				variants = append(variants, variant{name, r})
			}
		case scheduler.VariantBinpack, scheduler.VariantClassic:
			variants = append(variants, variant{name, 0})
		default:
			return fmt.Errorf("unknown policy %q (default, costmodel, binpack, classic)", name)
		}
	}
	seeds := []uint64{*seed}
	if *seedsFlag != "" {
		if seeds, err = parseSeeds(*seedsFlag); err != nil {
			return fmt.Errorf("-seeds: %w", err)
		}
	}
	if *noCache {
		os.Setenv("OFFSHORE_SIM_CACHE", "")
	}
	var inputs []simInput
	for _, src := range fs.Args() {
		in, err := loadInputs(src, seeds, *days, *tz, *cold)
		if err != nil {
			return err
		}
		inputs = append(inputs, in...)
	}
	type runMeta struct {
		in     int // index in inputs
		policy string
	}
	var specs []sim.Spec
	var metas []runMeta
	for k, in := range inputs {
		tr := in.trace
		tr.Jobs = slices.Clone(tr.Jobs)
		for i := range tr.Jobs {
			j := &tr.Jobs[i]
			if j.WorkingSet == 0 {
				j.WorkingSet = sim.Bytes(ws)
			}
			if *refetchSpread > 1 && j.Refetch == 0 {
				j.Refetch = repoRefetch(j.Repo, *refetch, *refetchSpread)
			}
			if j.SlotBytes == 0 && slot > 0 {
				j.SlotBytes, j.SlotGrowth = sim.Bytes(slot), sim.Bytes(slot/20)
			}
		}
		for _, v := range variants {
			pc := oc.Policy
			pc.Variant, pc.CostOptimization = v.name, v.r
			if pc.ExpiryMargin == 0 {
				pc.ExpiryMargin = 15 * time.Minute
			}
			if v.name == scheduler.VariantClassic {
				pc.ConsolidateBelow = 0
			}
			policy := v.name
			if v.name == scheduler.VariantDefault || v.name == scheduler.VariantCostModel {
				policy += fmt.Sprintf(" r=%.2g", v.r)
			}
			var e scheduler.Estimator = scheduler.Static{}
			if est == "history" {
				e = scheduler.NewHistory("")
			}
			specs = append(specs, sim.Spec{Trace: tr, Options: sim.Options{
				Name: in.label() + " " + policy, Policy: scheduler.Default{Cfg: pc}, Sizes: oc.Sizes, Estimator: e,
				Boot: bootLat, Start: startLat, MaxLifetime: oc.MaxLifetime, ColdPenalty: *cold,
				Swap: oc.Swap, QueueTimeout: *timeout, Resize: *resize, Refetch: *refetch, Down: in.down,
			}})
			metas = append(metas, runMeta{k, policy})
		}
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400) // short lived simulations: trade memory for less GC
	}
	began := time.Now()
	reports := sim.RunAll(specs, *parallel)
	fmt.Fprintf(os.Stderr, "sim: %d runs in %s (parallel %d)\n", len(specs), time.Since(began).Round(time.Millisecond), min(max(*parallel, 1), max(len(specs), 1)))
	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	case *table:
		rows := make([]tableRow, len(reports))
		for i, r := range reports {
			in := inputs[metas[i].in]
			rows[i] = tableRow{trace: in.name, seed: in.seed, seeded: in.seeded, policy: metas[i].policy, r: r}
		}
		printTable(os.Stdout, rows)
	case *sweep:
		for k, in := range inputs {
			printFrontier(os.Stdout, in.label(), ratios, reports[k*len(variants):(k+1)*len(variants)])
		}
	default:
		printReports(os.Stdout, reports)
	}
	return nil
}

// simInput is a trace to simulate: a file or a synthetic trace of a seed.
type simInput struct {
	name   string
	seed   uint64
	seeded bool // synthetic: seed matters
	trace  sim.Trace
	down   []sim.Outage
}

func (in simInput) label() string {
	if in.seeded {
		return fmt.Sprintf("%s seed=%d", in.name, in.seed)
	}
	return in.name
}

// loadInputs resolves a source of the sim command: a trace file (.json,
// .csv), an event log (.jsonl, converted like sim export), gen:PROFILE
// (a synthetic workday trace per seed), scenarios or scenario:NAME (the
// adversarial evaluation scenarios per seed). Files are parsed once and
// cached (sim.Cached).
func loadInputs(src string, seeds []uint64, days int, tz string, cold float64) ([]simInput, error) {
	switch {
	case strings.HasPrefix(src, "gen:"):
		profile := strings.TrimPrefix(src, "gen:")
		repos, ok := sim.Profiles[profile]
		if !ok {
			return nil, fmt.Errorf("%s: unknown profile %q (team, small)", src, profile)
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, err
		}
		var out []simInput
		for _, sd := range seeds {
			tr := sim.Generate(sim.GenOptions{Seed: sd, Days: days, Location: loc, Repos: repos,
				Start: time.Date(2026, 11, 9, 0, 0, 0, 0, loc)})
			out = append(out, simInput{name: src, seed: sd, seeded: true, trace: tr})
		}
		return out, nil
	case src == "scenarios" || strings.HasPrefix(src, "scenario:"):
		want := strings.TrimPrefix(src, "scenario:")
		var out []simInput
		for _, sd := range seeds {
			for _, sc := range sim.Scenarios(sd) {
				if src == "scenarios" || sc.Name == want {
					out = append(out, simInput{name: "scenario:" + sc.Name, seed: sd, seeded: true, trace: sc.Trace, down: sc.Down})
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: no such scenario", src)
		}
		return out, nil
	case strings.EqualFold(filepath.Ext(src), ".jsonl"):
		o := report.ExportOptions{ColdPenalty: max(cold, 0)}
		tr, err := sim.Cached(src, fmt.Sprintf("events cold=%g", o.ColdPenalty), func() (sim.Trace, error) {
			return report.SimTraceFile(src, o)
		})
		if err != nil {
			return nil, err
		}
		if len(tr.Jobs) == 0 {
			return nil, fmt.Errorf("%s: no finished tasks", src)
		}
		return []simInput{{name: src, trace: tr}}, nil
	default:
		tr, err := sim.Cached(src, "trace", func() (sim.Trace, error) { return sim.Load(src) })
		if err != nil {
			return nil, err
		}
		return []simInput{{name: src, trace: tr}}, nil
	}
}

// parseSeeds parses "1,2,5" and ranges "1-8".
func parseSeeds(s string) ([]uint64, error) {
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.ParseUint(lo, 10, 64)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.ParseUint(hi, 10, 64); err != nil {
				return nil, err
			}
		}
		if b < a || b-a > 10000 {
			return nil, fmt.Errorf("bad range %q", part)
		}
		for x := a; x <= b; x++ {
			out = append(out, x)
		}
	}
	return out, nil
}

type tableRow struct {
	trace  string
	seed   uint64
	seeded bool
	policy string
	r      sim.Report
}

// printTable prints one compact row per run and, for traces simulated
// with several seeds, the mean over the seeds per trace and policy.
func printTable(w io.Writer, rows []tableRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "trace\tseed\tpolicy\tcost $\tmach-h\tturn p50\tturn p95\twait p95\twarm\tcold\tflaps\tlate\tregret p95\tfailed\t")
	line := func(trace, seed, policy string, cost, mh float64, t50, t95, w95 time.Duration, warm, coldR, flaps, late float64, reg time.Duration, failed float64) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%.1f\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.3g\t%.3g\t%s\t%.3g\t\n", trace, seed, policy, cost, mh,
			t50.Round(time.Second), t95.Round(time.Second), w95.Round(time.Second), 100*warm, 100*coldR, flaps, late, reg.Round(time.Second), failed)
	}
	type key struct{ trace, policy string }
	var order []key
	groups := map[key][]sim.Report{}
	for _, x := range rows {
		seed := "-"
		if x.seeded {
			seed = strconv.FormatUint(x.seed, 10)
			k := key{x.trace, x.policy}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], x.r)
		}
		r := x.r
		line(x.trace, seed, x.policy, r.Cost, r.MachineHours, r.TurnP50, r.TurnP95, r.WaitP95, r.WarmRate, r.ColdRate,
			float64(r.Flaps), float64(r.Late), r.RegretP95, float64(r.Failed))
	}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		n := float64(len(g))
		mean := func(f func(sim.Report) float64) float64 {
			var s float64
			for _, r := range g {
				s += f(r)
			}
			return s / n
		}
		dur := func(f func(sim.Report) time.Duration) time.Duration {
			return time.Duration(mean(func(r sim.Report) float64 { return float64(f(r)) }))
		}
		line(k.trace, fmt.Sprintf("mean/%d", len(g)), k.policy,
			mean(func(r sim.Report) float64 { return r.Cost }), mean(func(r sim.Report) float64 { return r.MachineHours }),
			dur(func(r sim.Report) time.Duration { return r.TurnP50 }), dur(func(r sim.Report) time.Duration { return r.TurnP95 }),
			dur(func(r sim.Report) time.Duration { return r.WaitP95 }),
			mean(func(r sim.Report) float64 { return r.WarmRate }), mean(func(r sim.Report) float64 { return r.ColdRate }),
			mean(func(r sim.Report) float64 { return float64(r.Flaps) }), mean(func(r sim.Report) float64 { return float64(r.Late) }),
			dur(func(r sim.Report) time.Duration { return r.RegretP95 }), mean(func(r sim.Report) float64 { return float64(r.Failed) }))
	}
	tw.Flush()
}

// printReports writes reports as a table.
func printReports(w *os.File, rs []sim.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "run\tjobs\tdone\tcost $\tmachine-h\tcreated\twait p50\twait p95\twait max\tturn p50\tturn p95\tcpu util\tmem util\tslowdown\tOOMs\twarm\tflaps\tthrashed\tpage-in\tidle-h\tcold\tlate\tregret p95\t")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%.1f\t%d\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.2f\t%d\t%.0f%%\t%d\t%d\t%s\t%.1f\t%.0f%%\t%d\t%s\t\n",
			r.Name, r.Jobs, r.Completed, r.Cost, r.MachineHours, r.Machines,
			r.WaitP50.Round(time.Second), r.WaitP95.Round(time.Second), r.WaitMax.Round(time.Second),
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second),
			100*r.CPUUtil, 100*r.MemUtil, r.Slowdown, r.OOMs, 100*r.WarmRate, r.Flaps, r.Thrashed, gbytes(r.PageInBytes),
			r.IdleHours, 100*r.ColdRate, r.Late, r.RegretP95.Round(time.Second))
	}
	tw.Flush()
}

// printFrontier prints the cost/time curve of a cost_optimization sweep
// and marks the Pareto-optimal points (no other point is both cheaper and
// faster in p95 turnaround).
func printFrontier(w *os.File, path string, ratios []float64, rs []sim.Report) {
	fmt.Fprintf(w, "%s: cost_optimization sweep (1 = cheapest, 0 = fastest result)\n", path)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "cost_optimization\tcost $\tvolumes $\tmachine-h\tturn p50\tturn p95\twait p95\twarm\trestores\tattaches\tresizes\tthrashed\trefetch\tbias\tflaps\tsizes\tpareto\t")
	for i, r := range rs {
		pareto := "*"
		for j, o := range rs {
			if j != i && o.Cost <= r.Cost && o.TurnP95 <= r.TurnP95 && (o.Cost < r.Cost || o.TurnP95 < r.TurnP95) {
				pareto = ""
			}
		}
		fmt.Fprintf(tw, "%.1f\t%.2f\t%.2f\t%.1f\t%s\t%s\t%s\t%.0f%%\t%d\t%d\t%d\t%d\t%s\t%s\t%d\t%s\t%s\t\n", ratios[i], r.Cost, r.VolumeCost, r.MachineHours,
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second), r.WaitP95.Round(time.Second), 100*r.WarmRate, r.Restores, r.VolumeAttaches,
			r.Resizes, r.Thrashed, refetchOf(r.Refetch), biasOf(r.ThrashBias), r.Flaps, createdSizes(r.Created), pareto)
	}
	tw.Flush()
}

// refetchOf formats a calibrated refetch factor ("-" when no build
// thrashed).
func refetchOf(f float64) string {
	if f <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", f)
}

// biasOf formats the predicted/actual run ratio of thrashing builds
// ("-" when none thrashed).
func biasOf(b float64) string {
	if b <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", b)
}

// repoRefetch is a repository's true refetch factor for -refetch-spread:
// base (0: the scheduler default) times spread^u, u in [-1,1] from a hash
// of the name.
func repoRefetch(repo string, base, spread float64) float64 {
	if base <= 0 {
		base = scheduler.DefaultRefetch
	}
	h := fnv.New32a()
	h.Write([]byte(repo))
	u := float64(h.Sum32()%2001)/1000 - 1
	return base * math.Pow(spread, u)
}

// gbytes formats bytes as GB with one decimal ("-" for none).
func gbytes(b int64) string {
	if b <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
}

// createdSizes lists machines created per size ("big:3,small:1").
func createdSizes(m map[string]int) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, fmt.Sprintf("%s:%d", k, m[k]))
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

// latencies parses "60s" or "aws=90s,local=5s".
func latencies(s string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prov, v, ok := strings.Cut(part, "=")
		if !ok {
			prov, v = "", part
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, err
		}
		out[prov] = d
	}
	return out, nil
}
