package report

import (
	"bytes"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/scheduler"
	"github.com/traumventures/cloudsourced/internal/scheduler/sim"
)

const gi = int64(1) << 30

var t0 = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// fixture: machine m1 (4 CPU, 8G, $1/h) up from 22:00 on day 0 (before the
// window) to 02:00, then restarted 10:00-12:00; machine m2 ($2/h) created
// at 11:00 and still up at the window end (13:00). Two sessions on m1.
func fixture() []events.Event {
	capa := &scheduler.Resources{CPU: 4, Memory: 8 * gi}
	m1 := func(typ string, h float64) events.Event {
		return events.Event{Type: typ, Time: at(h), Machine: "m1", Provider: "aws", Size: "m4", Price: 1, Capacity: capa}
	}
	return []events.Event{
		m1(events.MachineCreate, -2), m1(events.MachineReady, -1.9),
		m1(events.MachineStop, 2),
		m1(events.MachineStart, 10),
		{Type: events.MachineState, Time: at(12), Machine: "m1", State: "stopped"}, // stopped itself
		{Type: events.MachineCreate, Time: at(11), Machine: "m2", Provider: "aws", Size: "m8", Price: 2, Capacity: &scheduler.Resources{CPU: 8, Memory: 16 * gi}},
		{Type: events.TaskPlaced, Time: at(10.1), Task: "a", Placement: "assign", WaitMs: 60000},
		{Type: events.TaskPlaced, Time: at(10.2), Task: "b", Placement: "assign", WaitMs: 1000},
		{Type: events.TaskPlaced, Time: at(10.3), Task: "c", Placement: "reject"},
		// a: 1h, half the machine reserved (2 CPU, 4G), used 1 CPU and 2G peak, cold.
		{Type: events.TaskEnd, Time: at(11.1), Task: "a", Repo: "r1", Env: "e1", Machine: "m1", Price: 1, Capacity: capa,
			Arrival: at(10.05), Need: &scheduler.Resources{CPU: 2, Memory: 4 * gi}, Reserve: &scheduler.Resources{CPU: 2, Memory: 4 * gi},
			Usage: &scheduler.Usage{Wall: time.Hour, CPUSeconds: 3600, PeakMemory: 2 * gi, Cache: map[string]int64{"sccache_hits": 3}}},
		// b: 30m, the whole machine, used 4 CPU, peak 10G (above estimate), warm, OOM.
		{Type: events.TaskEnd, Time: at(10.7), Task: "b", Repo: "r2", Env: "e1", Machine: "m1", Price: 1, Capacity: capa,
			Need: &scheduler.Resources{CPU: 4, Memory: 8 * gi}, Reserve: &scheduler.Resources{CPU: 4, Memory: 8 * gi},
			Usage: &scheduler.Usage{Wall: 30 * time.Minute, CPUSeconds: 7200, PeakMemory: 10 * gi, Warm: true, OOM: true}},
	}
}

func TestComputeCostAndUtilisation(t *testing.T) {
	from, to := at(0), at(13)
	r := Compute(fixture(), from, to)
	// m1: 0-2 (2h, clipped from 22:00) + 10-12 (2h) = 4h × $1; m2: 11-13 = 2h × $2.
	if !near(r.MachineHours, 6) || !near(r.Cost.Total, 8) || r.Machines != 2 {
		t.Fatalf("machine hours %v cost %v machines %d", r.MachineHours, r.Cost.Total, r.Machines)
	}
	// a: 1h × $1 × 0.5; b: 0.5h × $1 × 1.
	if !near(r.Cost.Attributed, 1) || !near(r.Cost.Idle, 7) {
		t.Fatalf("attributed %v idle %v", r.Cost.Attributed, r.Cost.Idle)
	}
	if len(r.Cost.ByRepo) != 2 || r.Cost.ByRepo[0].Key != "r1" && r.Cost.ByRepo[0].Key != "r2" || !near(r.Cost.ByRepo[0].Cost, 0.5) {
		t.Fatalf("by repo %+v", r.Cost.ByRepo)
	}
	if len(r.Cost.BySize) != 2 || r.Cost.BySize[0].Key != "aws/m4" && r.Cost.BySize[0].Key != "aws/m8" || !near(r.Cost.BySize[0].Cost, 4) {
		t.Fatalf("by size %+v", r.Cost.BySize)
	}
	if len(r.Cost.ByDay) != 1 || r.Cost.ByDay[0].Key != "2026-11-09" || !near(r.Cost.ByDay[0].Cost, 8) {
		t.Fatalf("by day %+v", r.Cost.ByDay)
	}
	u := r.Utilisation
	// Capacity: m1 4h × 4 CPU + m2 2h × 8 CPU = 32 core-h; reserved 2×1 + 4×0.5 = 4.
	if !near(u.CapacityCPUHours, 32) || !near(u.ReservedCPUHours, 4) || !near(u.ReservedCPU, 0.125) {
		t.Fatalf("cpu capacity/reserved %+v", u)
	}
	// Used CPU: 1h + 2h = 3 core-h of 4 reserved; memory: peak 2G×1h + 10G×0.5h = 7 of 4×1+8×0.5 = 8 GiB-h.
	if !near(u.UsedCPUHours, 3) || !near(u.OverprovisionCPU, 0.25) || !near(u.PeakMemGiBH, 7) || !near(u.OverprovisionMem, 0.125) {
		t.Fatalf("usage %+v", u)
	}
	if r.Tasks != 2 || r.Placements["assign"] != 2 || r.Placements["reject"] != 1 {
		t.Fatalf("tasks %d placements %v", r.Tasks, r.Placements)
	}
	if r.QueueWait.Count != 2 || r.QueueWait.P50 != time.Second || r.QueueWait.P95 != time.Minute || r.QueueWait.Max != time.Minute {
		t.Fatalf("queue wait %+v", r.QueueWait)
	}
	if r.Cache.Warm != 1 || r.Cache.Cold != 1 || !near(r.Cache.WarmRatio, 0.5) || r.Cache.Counters["sccache_hits"] != 3 {
		t.Fatalf("cache %+v", r.Cache)
	}
	e := r.Estimator
	// Ratios: 2/4 = 0.5 and 10/8 = 1.25; MAPE (2/2 + 2/10)/2 = 0.6.
	if e.Samples != 2 || e.Under != 1 || e.OOM != 1 || !near(e.RatioP50, 0.5) || !near(e.RatioP95, 1.25) || !near(e.RatioMean, 0.875) || !near(e.MAPE, 0.6) {
		t.Fatalf("estimator %+v", e)
	}
	// Busy cores / estimate: 1/2 and 4/4.
	if !near(e.CPURatio, 0.75) {
		t.Fatalf("cpu ratio %v", e.CPURatio)
	}
	var buf bytes.Buffer
	r.Write(&buf)
	for _, s := range []string{"$8.00 total", "$1.00 attributed", "aws/m4", "reserved 12% CPU", "p95 1m0s", "1 warm, 1 cold", "sccache 3 hits"} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("text report lacks %q:\n%s", s, buf.String())
		}
	}
}

func TestComputeSplitsDays(t *testing.T) {
	evs := []events.Event{
		{Type: events.MachineCreate, Time: at(20), Machine: "m", Price: 1},
		{Type: events.MachineDestroy, Time: at(30), Machine: "m"},
	}
	r := Compute(evs, at(0), at(48))
	if len(r.Cost.ByDay) != 2 || !near(r.Cost.ByDay[0].Cost, 4) || !near(r.Cost.ByDay[1].Cost, 6) || r.Utilisation.CapacityUnknownMH != 10 {
		t.Fatalf("days %+v unknown %v", r.Cost.ByDay, r.Utilisation.CapacityUnknownMH)
	}
}

func TestSimExportRoundTrip(t *testing.T) {
	tr := SimTrace(fixture(), time.Time{}, time.Time{}, ExportOptions{ColdPenalty: 0.5})
	if len(tr.Jobs) != 2 || !tr.Start.Equal(at(10)) {
		t.Fatalf("trace %+v", tr)
	}
	a, b := tr.Jobs[0], tr.Jobs[1]
	// a arrived 10:03 (explicit arrival); b has none: end - wall = 10:12.
	if a.Repo != "r1" || !near(a.At, 180) || !near(a.Duration, 2400) || !near(a.CPU, 1) || a.Memory != sim.Bytes(4*gi) || a.PeakMemory != sim.Bytes(2*gi) {
		t.Fatalf("job a %+v", a)
	}
	if b.Repo != "r2" || !near(b.At, 720) || b.Env != "e1" || !near(b.Duration, 1800) || !near(b.CPU, 4) {
		t.Fatalf("job b %+v", b)
	}
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := sim.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Start.Equal(tr.Start) || len(back.Jobs) != 2 {
		t.Fatalf("loaded %+v", back)
	}
	for i := range tr.Jobs {
		if back.Jobs[i] != tr.Jobs[i] {
			t.Fatalf("job %d: %+v != %+v", i, back.Jobs[i], tr.Jobs[i])
		}
	}
	// The simulator accepts the trace.
	rep := sim.Run(back, sim.Options{Name: "export", Policy: scheduler.Default{Cfg: scheduler.Config{MaxMachines: 2, CPUOvercommit: 1, MemoryOvercommit: 1, ExpiryMargin: 15 * time.Minute}},
		Sizes: []scheduler.Size{{Name: "m16", Capacity: scheduler.Resources{CPU: 16, Memory: 32 * gi}, PricePerHour: 1}}, Estimator: scheduler.Static{}})
	if rep.Jobs != 2 || rep.Completed != 2 {
		t.Fatalf("simulation %+v", rep)
	}
	req := SimTrace(fixture(), time.Time{}, time.Time{}, ExportOptions{RequestCPU: true})
	if !near(req.Jobs[0].CPU, 2) || !near(req.Jobs[1].CPU, 4) {
		t.Fatalf("request cpu %+v", req.Jobs)
	}
}

func TestMachineSamplesAndContention(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	capa := &scheduler.Resources{CPU: 8, Memory: 16 << 30}
	sample := func(min int, busy float64, sessions int) events.Event {
		return events.Event{Time: t0.Add(time.Duration(min) * time.Minute), Type: events.MachineSample, Machine: "m", Provider: "aws", Size: "x8",
			Capacity: capa, Reserved: &scheduler.Resources{CPU: 4}, Used: &scheduler.Resources{CPU: busy, Memory: 4 << 30},
			Pressure: &scheduler.Pressure{CPU: 0.1}, Sessions: sessions}
	}
	evs := []events.Event{sample(1, 8, 2), sample(2, 4, 1), sample(3, 0, 0),
		{Time: t0.Add(4 * time.Minute), Type: events.TaskEnd, Usage: &scheduler.Usage{Wall: 100 * time.Second, CPUStall: 25}}}
	r := Compute(evs, t0, t0.Add(time.Hour))
	if len(r.Samples) != 1 {
		t.Fatalf("samples %+v", r.Samples)
	}
	s := r.Samples[0]
	if s.Size != "aws/x8" || s.Samples != 3 || s.BusyCPUMean != 0.5 || s.SessionsMax != 2 || s.ReservedCPU != 0.5 || s.MemoryMean != 0.25 {
		t.Fatalf("%+v", s)
	}
	if r.CPUStallShare != 0.25 {
		t.Fatalf("stall %v", r.CPUStallShare)
	}
}
