package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/pool"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/scheduler/sim"
)

const simUsage = `Usage:
  offshore sim [flags] <trace.json|trace.csv>...
      replay traces against the pool configuration and compare policies
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
	policies := fs.String("policy", "default,binpack,classic", "comma separated policies to compare (default, binpack, classic)")
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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
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
		case scheduler.VariantDefault:
			for _, r := range ratios {
				variants = append(variants, variant{name, r})
			}
		case scheduler.VariantBinpack, scheduler.VariantClassic:
			variants = append(variants, variant{name, 0})
		default:
			return fmt.Errorf("unknown policy %q (default, binpack, classic)", name)
		}
	}
	var reports []sim.Report
	for _, path := range fs.Args() {
		tr, err := sim.Load(path)
		if err != nil {
			return err
		}
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
		runs := make([]sim.Report, len(variants))
		var wg sync.WaitGroup
		for i, v := range variants {
			pc := oc.Policy
			pc.Variant, pc.CostOptimization = v.name, v.r
			if pc.ExpiryMargin == 0 {
				pc.ExpiryMargin = 15 * time.Minute
			}
			if v.name == scheduler.VariantClassic {
				pc.ConsolidateBelow = 0
			}
			label := path + " " + v.name
			if v.name == scheduler.VariantDefault {
				label += fmt.Sprintf(" r=%.2g", v.r)
			}
			var e scheduler.Estimator = scheduler.Static{}
			if est == "history" {
				e = scheduler.NewHistory("")
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				runs[i] = sim.Run(tr, sim.Options{
					Name: label, Policy: scheduler.Default{Cfg: pc}, Sizes: oc.Sizes, Estimator: e,
					Boot: bootLat, Start: startLat, MaxLifetime: oc.MaxLifetime, ColdPenalty: *cold,
					Swap: oc.Swap, QueueTimeout: *timeout, Resize: *resize, Refetch: *refetch,
				})
			}()
		}
		wg.Wait()
		reports = append(reports, runs...)
		if *sweep && !*asJSON {
			printFrontier(os.Stdout, path, ratios, runs)
		}
	}
	if *sweep && !*asJSON {
		return nil
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}
	printReports(os.Stdout, reports)
	return nil
}

// printReports writes reports as a table.
func printReports(w *os.File, rs []sim.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "run\tjobs\tdone\tcost $\tmachine-h\tcreated\twait p50\twait p95\twait max\tturn p50\tturn p95\tcpu util\tmem util\tslowdown\tOOMs\twarm\tflaps\tthrashed\tpage-in\t")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%.1f\t%d\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.2f\t%d\t%.0f%%\t%d\t%d\t%s\t\n",
			r.Name, r.Jobs, r.Completed, r.Cost, r.MachineHours, r.Machines,
			r.WaitP50.Round(time.Second), r.WaitP95.Round(time.Second), r.WaitMax.Round(time.Second),
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second),
			100*r.CPUUtil, 100*r.MemUtil, r.Slowdown, r.OOMs, 100*r.WarmRate, r.Flaps, r.Thrashed, gbytes(r.PageInBytes))
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
