package report

import (
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/labels"
)

// GroupKeys are the dimensions of `cloudsourced report --by`.
var GroupKeys = []string{labels.Agent, labels.Worktree, labels.Branch, labels.Repo, labels.User}

// DefaultGroups are shown when --by is not given.
var DefaultGroups = []string{labels.Agent, labels.Worktree}

// Group summarises the sessions of one label value.
type Group struct {
	Key          string        `json:"key"`
	Sessions     int           `json:"sessions"` // finished with usage
	Prewarms     int           `json:"prewarms"` // of which purpose=prewarm
	Requests     int           `json:"requests"` // placement decisions
	Failed       int           `json:"failed"`   // reject, timeout, error
	SessionHours float64       `json:"session_hours"`
	Cost         float64       `json:"cost"` // attributed machine cost
	CPUHours     float64       `json:"cpu_hours"`
	WaitP50      time.Duration `json:"wait_p50"`
	WaitP95      time.Duration `json:"wait_p95"`
	WaitMax      time.Duration `json:"wait_max"`
	// CPUStallShare and MemoryStallShare are the shares of wall time in
	// which the group's sessions waited for CPU or memory (contention).
	CPUStallShare    float64 `json:"cpu_stall_share"`
	MemoryStallShare float64 `json:"memory_stall_share"`
}

// Grouping is a report dimension with its groups (by cost, descending).
type Grouping struct {
	By     string  `json:"by"`
	Groups []Group `json:"groups"`
}

// groupValue returns the value of dimension by for a task event. Events
// written before labels existed fall back to the repository id for "repo"
// and "worktree"; other dimensions are "(none)".
func groupValue(ev events.Event, by string) string {
	if v := ev.Labels[by]; v != "" {
		return v
	}
	if (by == labels.Repo || by == labels.Worktree) && ev.Repo != "" {
		return ev.Repo
	}
	if by == labels.Agent {
		return "(human)"
	}
	return "(none)"
}

// taskShare is the clipped session-hours, attributed cost and CPU hours
// of a task_end event in [from, to).
func taskShare(ev events.Event, from, to time.Time) (hours, cost, cpuHours, frac float64, ok bool) {
	u := ev.Usage
	if u == nil || u.Wall <= 0 {
		return 0, 0, 0, 0, false
	}
	end := ev.Time
	s, e, in := clip(end.Add(-u.Wall), end, from, to)
	if !in {
		return 0, 0, 0, 0, false
	}
	hours = e.Sub(s).Hours()
	frac = e.Sub(s).Seconds() / u.Wall.Seconds()
	share := 1.0
	if res := ev.Reserve; res != nil {
		if c := ev.Capacity; c != nil && c.CPU > 0 && c.Memory > 0 {
			share = math.Min(1, (res.CPU/c.CPU+float64(res.Memory)/float64(c.Memory))/2)
		}
	}
	return hours, hours * ev.Price * share, u.CPUSeconds / 3600 * frac, frac, true
}

// Groups groups the task events of [from, to) by a label dimension.
func Groups(evs []events.Event, from, to time.Time, by string) Grouping {
	type acc struct {
		g                Group
		waits            []time.Duration
		wall, cpuS, memS float64
	}
	m := map[string]*acc{}
	get := func(k string) *acc {
		a := m[k]
		if a == nil {
			a = &acc{g: Group{Key: k}}
			m[k] = a
		}
		return a
	}
	for _, ev := range evs {
		switch ev.Type {
		case events.TaskPlaced:
			if ev.Time.Before(from) || !ev.Time.Before(to) {
				continue
			}
			a := get(groupValue(ev, by))
			a.g.Requests++
			switch ev.Placement {
			case "assign":
				a.waits = append(a.waits, time.Duration(ev.WaitMs)*time.Millisecond)
			case "reject", "timeout", "error":
				a.g.Failed++
			}
		case events.TaskEnd:
			if ev.Usage == nil || ev.Time.Before(from) || !ev.Time.Before(to) {
				continue
			}
			a := get(groupValue(ev, by))
			a.g.Sessions++
			if ev.Labels[labels.Purpose] == labels.PurposePrewarm {
				a.g.Prewarms++
			}
			if h, cost, cpuH, frac, ok := taskShare(ev, from, to); ok {
				a.g.SessionHours += h
				a.g.Cost += cost
				a.g.CPUHours += cpuH
				a.wall += ev.Usage.Wall.Seconds() * frac
				a.cpuS += ev.Usage.CPUStall * frac
				a.memS += ev.Usage.MemoryStall * frac
			}
		}
	}
	out := Grouping{By: by}
	for _, a := range m {
		w := waitStats(a.waits)
		a.g.WaitP50, a.g.WaitP95, a.g.WaitMax = w.P50, w.P95, w.Max
		a.g.CPUStallShare, a.g.MemoryStallShare = div(a.cpuS, a.wall), div(a.memS, a.wall)
		out.Groups = append(out.Groups, a.g)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		gi, gj := out.Groups[i], out.Groups[j]
		if gi.Cost != gj.Cost {
			return gi.Cost > gj.Cost
		}
		if gi.SessionHours != gj.SessionHours {
			return gi.SessionHours > gj.SessionHours
		}
		return gi.Key < gj.Key
	})
	return out
}

// WriteGroups prints groupings for humans.
func WriteGroups(w io.Writer, gs []Grouping) {
	for _, g := range gs {
		if len(g.Groups) == 0 {
			continue
		}
		fmt.Fprintf(w, "\nby %s\n", g.By)
		tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  KEY\tCOST $\tSESSION-H\tCPU-H\tSESSIONS\tREQUESTS\tFAILED\tWAIT p50/p95/max\tSTALL CPU/MEM")
		for _, x := range g.Groups {
			sess := fmt.Sprint(x.Sessions)
			if x.Prewarms > 0 {
				sess += fmt.Sprintf(" (%d prewarm)", x.Prewarms)
			}
			fmt.Fprintf(tw, "  %s\t%.2f\t%.2f\t%.2f\t%s\t%d\t%d\t%s / %s / %s\t%.0f%% / %.0f%%\n", x.Key, x.Cost, x.SessionHours, x.CPUHours,
				sess, x.Requests, x.Failed, x.WaitP50.Round(time.Millisecond), x.WaitP95.Round(time.Millisecond), x.WaitMax.Round(time.Millisecond),
				100*x.CPUStallShare, 100*x.MemoryStallShare)
		}
		tw.Flush()
	}
}
