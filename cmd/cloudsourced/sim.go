package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/pool"
	"github.com/traumventures/cloudsourced/internal/scheduler"
	"github.com/traumventures/cloudsourced/internal/scheduler/sim"
)

const simUsage = `Usage:
  cloudsourced sim [flags] <trace.json|trace.csv>...
      replay traces against the pool configuration and compare policies
  cloudsourced sim -generate out.json [-profile team|small] [-seed N] [-days N] [-tz ZONE]
      write a synthetic bursty workday trace
  cloudsourced sim export [-since 7d] [-until T] [-o trace.json] [-cold-penalty 0.5] [-request-cpu]
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
	policies := fs.String("policy", "default,classic", "comma separated policies to compare (default, binpack, classic)")
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
	var reports []sim.Report
	for _, path := range fs.Args() {
		tr, err := sim.Load(path)
		if err != nil {
			return err
		}
		for _, name := range strings.Split(*policies, ",") {
			name = strings.TrimSpace(name)
			pc := oc.Policy
			pc.Variant = name
			if pc.ExpiryMargin == 0 {
				pc.ExpiryMargin = 15 * time.Minute
			}
			if name == scheduler.VariantClassic {
				pc.ConsolidateBelow = 0
			} else if name != scheduler.VariantDefault && name != "binpack" {
				return fmt.Errorf("unknown policy %q (default, binpack, classic)", name)
			}
			var e scheduler.Estimator = scheduler.Static{}
			if est == "history" {
				e = scheduler.NewHistory("")
			}
			r := sim.Run(tr, sim.Options{
				Name: path + " " + name, Policy: scheduler.Default{Cfg: pc}, Sizes: oc.Sizes, Estimator: e,
				Boot: bootLat, Start: startLat, MaxLifetime: oc.MaxLifetime, ColdPenalty: *cold,
				Swap: oc.Swap, QueueTimeout: *timeout,
			})
			reports = append(reports, r)
		}
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
	fmt.Fprintln(tw, "run\tjobs\tdone\tcost $\tmachine-h\tcreated\twait p50\twait p95\twait max\tturn p50\tturn p95\tcpu util\tmem util\tslowdown\tOOMs\twarm\t")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%.1f\t%d\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.2f\t%d\t%.0f%%\t\n",
			r.Name, r.Jobs, r.Completed, r.Cost, r.MachineHours, r.Machines,
			r.WaitP50.Round(time.Second), r.WaitP95.Round(time.Second), r.WaitMax.Round(time.Second),
			r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second),
			100*r.CPUUtil, 100*r.MemUtil, r.Slowdown, r.OOMs, 100*r.WarmRate)
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
