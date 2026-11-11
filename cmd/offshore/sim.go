package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
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
					Swap: oc.Swap, QueueTimeout: *timeout, Resize: *resize,
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
	fmt.Fprintln(tw, "run\tjobs\tdone\tcost $\tmachine-h\tcreated\twait p50\twait p95\twait max\tturn p50\tturn p95\tcpu util\tmem util\tslowdown\tOOMs\twarm\tflaps\t")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%.1f\t%d\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.2f\t%d\t%.0f%%\t%d\t\n",
			r.Name, r.Jobs, r.Completed, r.Cost, r.MachineHours, r.Machines,
			r.WaitP50.Round(time.Second), r.WaitP95.Round(time.Second), r.WaitMax.Round(time.Second),
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second),
			100*r.CPUUtil, 100*r.MemUtil, r.Slowdown, r.OOMs, 100*r.WarmRate, r.Flaps)
	}
	tw.Flush()
}

// printFrontier prints the cost/time curve of a cost_optimization sweep
// and marks the Pareto-optimal points (no other point is both cheaper and
// faster in p95 turnaround).
func printFrontier(w *os.File, path string, ratios []float64, rs []sim.Report) {
	fmt.Fprintf(w, "%s: cost_optimization sweep (1 = cheapest, 0 = fastest result)\n", path)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "cost_optimization\tcost $\tmachine-h\tturn p50\tturn p95\twait p95\twarm\trestores\tflaps\tpareto\t")
	for i, r := range rs {
		pareto := "*"
		for j, o := range rs {
			if j != i && o.Cost <= r.Cost && o.TurnP95 <= r.TurnP95 && (o.Cost < r.Cost || o.TurnP95 < r.TurnP95) {
				pareto = ""
			}
		}
		fmt.Fprintf(tw, "%.1f\t%.2f\t%.1f\t%s\t%s\t%s\t%.0f%%\t%d\t%d\t%s\t\n", ratios[i], r.Cost, r.MachineHours,
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second), r.WaitP95.Round(time.Second), 100*r.WarmRate, r.Restores, r.Flaps, pareto)
	}
	tw.Flush()
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
