package report

import (
	"math"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/scheduler/sim"
)

// ExportOptions controls the conversion of task events into a trace.
type ExportOptions struct {
	// ColdPenalty is the simulator's cold build penalty: the duration of a
	// run without a warm build cache is divided by 1+ColdPenalty, so that
	// the simulator (which adds the penalty again on cold machines) sees
	// the warm duration. 0 or negative: no correction.
	ColdPenalty float64
	// RequestCPU uses the requested CPU instead of the measured busy cores.
	RequestCPU bool
}

// SimTrace converts finished tasks of [from, to) into a simulator trace.
//
// Normalisation: the simulator runs a job for Duration seconds at CPU
// cores when it gets its full CPU. A job's CPU is therefore the measured
// average of busy cores (CPU seconds / wall), so that Duration × CPU
// reproduces the measured CPU time, and Duration is the wall time (warm
// equivalent for cold runs, see ExportOptions). Runs without CPU
// measurement keep their requested CPU. Memory is the request (the
// estimate the orchestrator packed with) and PeakMemory the measured peak.
// Arrival times are the requests' arrival; the trace starts at the hour
// of the first arrival.
func SimTrace(evs []events.Event, from, to time.Time, o ExportOptions) sim.Trace {
	type job struct {
		at time.Time
		j  sim.Job
	}
	var jobs []job
	// task_end events reported by a remote daemon carry only the task id
	// and usage: take repository, environment, arrival and need from the
	// task's task_placed event.
	placed := map[string]events.Event{}
	for _, ev := range evs {
		if ev.Type == events.TaskPlaced && ev.Task != "" {
			placed[ev.Task] = ev
		}
	}
	for _, ev := range evs {
		u := ev.Usage
		if ev.Type != events.TaskEnd || u == nil || u.Wall <= 0 {
			continue
		}
		if p, ok := placed[ev.Task]; ok {
			if ev.Repo == "" {
				ev.Repo = p.Repo
			}
			if ev.Env == "" {
				ev.Env = p.Env
			}
			if ev.Arrival.IsZero() {
				ev.Arrival, ev.WaitMs = p.Arrival, p.WaitMs
			}
			if ev.Need == nil {
				ev.Need = p.Need
			}
		}
		if (!from.IsZero() && ev.Time.Before(from)) || (!to.IsZero() && !ev.Time.Before(to)) {
			continue
		}
		at := ev.Arrival
		if at.IsZero() {
			at = ev.Time.Add(-u.Wall - time.Duration(ev.WaitMs)*time.Millisecond)
		}
		wall := u.Wall.Seconds()
		if !u.Warm && o.ColdPenalty > 0 {
			wall /= 1 + o.ColdPenalty
		}
		cpu := 1.0
		var mem int64
		if ev.Need != nil {
			if ev.Need.CPU > 0 {
				cpu = ev.Need.CPU
			}
			mem = ev.Need.Memory
		}
		if !o.RequestCPU && u.CPUSeconds > 0 {
			cpu = math.Max(0.05, u.CPUSeconds/u.Wall.Seconds())
		}
		jobs = append(jobs, job{at, sim.Job{
			Repo: ev.Repo, Env: ev.Env, Duration: round(wall, 3), CPU: round(cpu, 2),
			Memory: sim.Bytes(mem), PeakMemory: sim.Bytes(u.PeakMemory),
			SlotBytes: sim.Bytes(u.SlotBytesAfter), SlotGrowth: sim.Bytes(slotGrowth(u)),
		}})
	}
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].at.Before(jobs[k].at) })
	t := sim.Trace{Start: sim.DefaultStart, Jobs: []sim.Job{}}
	if len(jobs) == 0 {
		return t
	}
	t.Start = jobs[0].at.UTC().Truncate(time.Hour)
	for _, j := range jobs {
		j.j.At = round(j.at.Sub(t.Start).Seconds(), 3)
		t.Jobs = append(t.Jobs, j.j)
		if end := j.j.At + j.j.Duration; end > t.End {
			t.End = math.Ceil(end)
		}
	}
	return t
}

// slotGrowth is what a run on an existing slot added to it.
func slotGrowth(u *scheduler.Usage) int64 {
	if u.SlotBytesBefore <= 0 || u.SlotBytesAfter <= u.SlotBytesBefore {
		return 0
	}
	return u.SlotBytesAfter - u.SlotBytesBefore
}

func round(x float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(x*p) / p
}
