package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/traumventures/cloudsourced/internal/daemon"
	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/labels"
)

const whyUsage = "usage: cloudsourced why [N|last|list] [--json]  (N=1 is the most recent run)"

// why explains a recent proxied run from the daemon's run log.
func why(args []string) error {
	asJSON, list, n := false, false, 1
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case a == "last":
			n = 1
		case a == "list":
			list = true
		default:
			v, err := strconv.Atoi(a)
			if err != nil || v < 1 {
				return errors.New(whyUsage)
			}
			n = v
		}
	}
	runs, err := events.Recent(daemon.StateDir(), 0)
	if err != nil {
		return err
	}
	if list {
		if asJSON {
			if runs == nil {
				runs = []events.Run{}
			}
			return writeJSON(os.Stdout, runs)
		}
		printRunList(os.Stdout, runs)
		return nil
	}
	if len(runs) < n {
		if len(runs) == 0 {
			return errors.New("no runs recorded yet")
		}
		return fmt.Errorf("only %d runs recorded", len(runs))
	}
	if asJSON {
		return writeJSON(os.Stdout, runs[n-1])
	}
	printRun(os.Stdout, runs[n-1])
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func commandLine(r events.Run) string {
	return strings.TrimSpace(r.Tool + " " + strings.Join(r.Args, " "))
}

func printRunList(w io.Writer, runs []events.Run) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "no runs recorded yet")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "N\tTIME\tMODE\tAGENT\tMACHINE\tEXIT\tTOTAL\tCOMMAND")
	for i, r := range runs {
		exit := "-"
		if r.Exit != nil {
			exit = strconv.Itoa(*r.Exit)
		}
		machine := r.Machine
		if machine == "" {
			machine = "-"
		}
		mode := r.Mode
		if r.Labels[labels.Purpose] == labels.PurposePrewarm {
			mode += " (prewarm)"
		}
		agent := r.Labels[labels.Agent]
		if agent == "" {
			agent = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, r.Time.Local().Format("Jan 02 15:04:05"), mode, agent, machine, exit,
			ms(r.Timings.Total), commandLine(r))
	}
	tw.Flush()
}

func ms(v int64) string { return (time.Duration(v) * time.Millisecond).String() }

// printRun prints one run human readable.
func printRun(w io.Writer, r events.Run) {
	fmt.Fprintf(w, "%s  %s\n", r.Time.Local().Format("2006-01-02 15:04:05"), commandLine(r))
	if r.Cwd != "" {
		fmt.Fprintf(w, "  directory  %s\n", r.Cwd)
	}
	fmt.Fprintf(w, "  ran        %s: %s\n", r.Mode, r.Why)
	if len(r.Labels) > 0 {
		fmt.Fprintf(w, "  labels     %s\n", labels.Format(r.Labels))
	}
	if r.Machine != "" {
		m := r.Machine
		if r.Provider != "" || r.Size != "" {
			m += fmt.Sprintf(" (%s/%s", r.Provider, r.Size)
			if r.Price > 0 {
				m += fmt.Sprintf(", %.3f/h", r.Price)
			}
			m += ")"
		}
		fmt.Fprintf(w, "  machine    %s\n", m)
	}
	if r.Placement != "" {
		fmt.Fprintf(w, "  placement  %s\n", r.Placement)
	}
	if r.Slot != "" {
		state := "cold"
		if r.Warm {
			state = "warm"
		}
		fmt.Fprintf(w, "  build slot %s (%s)\n", r.Slot, state)
	}
	if len(r.Ports) > 0 {
		fmt.Fprintf(w, "  ports      %s\n", strings.Join(r.Ports, ", "))
	}
	if r.Exit != nil {
		fmt.Fprintf(w, "  exit       %d\n", *r.Exit)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "  error      %s\n", r.Error)
	}
	t := r.Timings
	if r.Mode == events.Remote {
		fmt.Fprintln(w, "  timings")
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, p := range []struct {
			name string
			v    int64
		}{
			{"environment image", t.Env}, {"local sync", t.Sync}, {"queue / machine start", t.Queue},
			{"attach roots", t.Attach}, {"connect", t.Connect}, {"worker barrier", t.Barrier},
			{"run", t.Run}, {"write back", t.Writeback}, {"outputs", t.Outputs}, {"total", t.Total},
		} {
			fmt.Fprintf(tw, "    %s\t%s\n", p.name, ms(p.v))
		}
		tw.Flush()
	} else if t.Total > 0 {
		fmt.Fprintf(w, "  decided in %s\n", ms(t.Total))
	}
	if u := r.Usage; u != nil {
		line := fmt.Sprintf("peak memory %s, cpu %.1fs", human(u.PeakMemory), u.CPUSeconds)
		if u.OOM {
			line += ", OOM killed"
		}
		fmt.Fprintf(w, "  usage      %s\n", line)
	}
	if len(r.Cache) > 0 {
		keys := make([]string, 0, len(r.Cache))
		for k := range r.Cache {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Cache[k]))
		}
		fmt.Fprintf(w, "  cache      %s\n", strings.Join(parts, " "))
	}
}
