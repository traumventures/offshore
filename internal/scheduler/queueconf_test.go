package scheduler

import (
	"strings"
	"testing"
	"time"
)

// queuePool: a busy warm machine whose session has run 5 minutes (median
// run medRun, 80th percentile p80), an idle cold machine with room, and no
// way to add a machine (the hedge bound does not apply).
func queuePool(medRun, p80 time.Duration) (Pool, Task) {
	b := busy("warm", res(8, 16), res(8, 16), 1)
	b.Size, b.WarmRepos = "m", []string{"r"}
	start := now.Add(-5 * time.Minute)
	b.Releases = []Release{{At: start.Add(p80), Start: start, Run: medRun, Res: res(8, 16)}}
	idle := machine("idle", res(8, 16), Resources{})
	idle.Size = "m"
	tk := task(4, 8)
	tk.Profile = Profile{Warm: 5 * time.Minute, Cold: 7 * time.Minute, Spread: 1.1}
	return Pool{Now: now, Sizes: priced, Machines: []Machine{b, idle}}, tk
}

func queueCfg(conf float64) Config {
	return Config{CostOptimization: 0, CPUOvercommit: 1, MemoryOvercommit: 1, MaxSpendPerHour: 0.01, QueueConfidence: conf}
}

// A session without a lease estimate is not counted on to end: run now.
func TestQueueUnknownRemainingRunsNow(t *testing.T) {
	f, tk := queuePool(6*time.Minute, 7*time.Minute)
	f.Machines[0].Releases[0].Unknown = true
	pl := Default{Cfg: queueCfg(0)}.Place(f, tk)
	if pl.Kind != Assign || pl.Machine != "idle" || !strings.Contains(pl.Reason, "run now: p=0.00") {
		t.Fatalf("want run now on idle, got %+v", pl)
	}
	if v := pl.Decision.Queue; v == nil || v.WaitFinishSeconds != -1 || v.Confidence != DefaultQueueConfidence {
		t.Fatalf("verdict: %+v", v)
	}
}

// A session nearly done with a tight run time distribution: waiting for
// the warm slot is confidently faster than a cold run.
func TestQueueConfidentWait(t *testing.T) {
	f, tk := queuePool(5*time.Minute+20*time.Second, 5*time.Minute+40*time.Second)
	pl := Default{Cfg: queueCfg(0)}.Place(f, tk)
	if pl.Kind != Wait || !strings.Contains(pl.Reason, "wait: p=") || pl.Decision.Queue.P < 0.8 {
		t.Fatalf("want wait, got %+v %+v", pl, pl.Decision.Queue)
	}
}

// A session whose remaining time is uncertain: the median says waiting
// is a little faster, but not with 80 % confidence; a lower configured
// confidence waits.
func TestQueueUncertainWaitRunsNow(t *testing.T) {
	f, tk := queuePool(5*time.Minute+30*time.Second, 9*time.Minute)
	pl := Default{Cfg: queueCfg(0)}.Place(f, tk)
	v := pl.Decision.Queue
	if pl.Kind != Assign || v == nil || v.P >= 0.8 || v.P <= 0.3 {
		t.Fatalf("want run now with 0.3 < p < 0.8, got %+v %+v", pl, v)
	}
	if pl := (Default{Cfg: queueCfg(0.3)}).Place(f, tk); pl.Kind != Wait {
		t.Fatalf("queue_confidence 0.3: want wait, got %+v %+v", pl, pl.Decision.Queue)
	}
}

// Scale-out: a queued request behind a session of unknown remaining time
// gets a machine; behind one about to end it waits.
func TestQueueScaleOut(t *testing.T) {
	b := busy("warm", res(8, 16), res(8, 16), 1)
	b.Size, b.WarmRepos = "m", []string{"r"}
	start := now.Add(-5 * time.Minute)
	b.Releases = []Release{{At: start.Add(5*time.Minute + 30*time.Second), Start: start, Run: 5*time.Minute + 15*time.Second, Res: res(8, 16)}}
	tk := task(4, 8)
	tk.Since = now
	tk.Profile = Profile{Warm: 5 * time.Minute, Cold: 7 * time.Minute, Spread: 1.1}
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{b}, Queue: []Task{tk}, Providers: map[string]ProviderState{"p": {Boot: time.Minute}}}
	pol := Default{Cfg: Config{CostOptimization: 0.2, CPUOvercommit: 1, MemoryOvercommit: 1, QueueTimeout: 10 * time.Minute}}
	if acts := pol.Plan(f); len(acts) != 0 {
		t.Fatalf("session ends in 30s: want no machine, got %+v", acts)
	}
	if pl := pol.Place(f, tk); pl.Kind != Wait || pl.Decision.Queue == nil || pl.Decision.Queue.Verdict != "wait" {
		t.Fatalf("place verdict: %+v", pl.Decision.Queue)
	}
	f.Machines[0].Releases[0].Unknown = true
	acts := pol.Plan(f)
	if len(acts) != 1 || acts[0].Kind != Create || acts[0].Decision.Queue == nil || acts[0].Decision.Queue.Verdict != "create now" {
		t.Fatalf("unknown remaining time: want a create, got %+v", acts)
	}
}
