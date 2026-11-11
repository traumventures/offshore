package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/report"
)

// flushTelemetry runs a tracing shutdown function with a deadline.
func flushTelemetry(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdown(ctx)
}

// fetchEvents reads the pool's event log through the daemon (embedded:
// its state directory; remote: the control plane's /v1/events).
func fetchEvents(since, until time.Time) ([]events.Event, error) {
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return nil, err
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "events", Since: since, Until: until}, os.Stderr)
	if err != nil {
		return nil, err
	}
	var evs []events.Event
	err = json.Unmarshal(raw, &evs)
	return evs, err
}

// window parses --since/--until.
func window(since, until string) (from, to time.Time, err error) {
	now := time.Now()
	to = now
	if until != "" {
		if to, err = events.ParseSince(until, now); err != nil {
			return from, to, fmt.Errorf("--until: %w", err)
		}
	}
	if from, err = events.ParseSince(since, now); err != nil {
		return from, to, fmt.Errorf("--since: %w", err)
	}
	if !from.Before(to) {
		return from, to, errors.New("--since must be before --until")
	}
	return from, to, nil
}

func reportCmd(args []string) error {
	fs := newFlagSet("report")
	since := fs.String("since", "7d", "start of the window: 7d, 36h, 2026-11-01 or RFC 3339")
	until := fs.String("until", "", "end of the window (default now; same formats, durations count back from now)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	by := fs.String("by", "", "group sessions by label: "+strings.Join(report.GroupKeys, ", ")+" (comma separated; default agent,worktree)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	from, to, err := window(*since, *until)
	if err != nil {
		return err
	}
	evs, err := fetchEvents(from.Add(-report.Lookback), to)
	if err != nil {
		return err
	}
	r := report.Compute(evs, from, to)
	if *by != "" {
		r.Groupings = nil
		for _, k := range strings.Split(*by, ",") {
			k = strings.TrimSpace(k)
			if !slices.Contains(report.GroupKeys, k) {
				return fmt.Errorf("--by %q: want one of %s", k, strings.Join(report.GroupKeys, ", "))
			}
			r.Groupings = append(r.Groupings, report.Groups(evs, from, to, k))
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	r.Write(os.Stdout)
	return nil
}

func simExport(args []string) error {
	fs := newFlagSet("sim export")
	since := fs.String("since", "7d", "start of the window: 7d, 36h, 2026-11-01 or RFC 3339")
	until := fs.String("until", "", "end of the window (default now)")
	out := fs.String("o", "trace.json", "output trace (JSON)")
	cold := fs.Float64("cold-penalty", 0.5, "divide cold runs by 1+penalty (the simulator's -cold-penalty; 0 = keep)")
	request := fs.Bool("request-cpu", false, "use the requested CPU instead of the measured busy cores")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	from, to, err := window(*since, *until)
	if err != nil {
		return err
	}
	evs, err := fetchEvents(from, to)
	if err != nil {
		return err
	}
	tr := report.SimTrace(evs, from, to, report.ExportOptions{ColdPenalty: *cold, RequestCPU: *request})
	if err := tr.Save(*out); err != nil {
		return err
	}
	fmt.Printf("wrote %d jobs to %s (from %s)\n", len(tr.Jobs), *out, tr.Start.Format(time.RFC3339))
	return nil
}
