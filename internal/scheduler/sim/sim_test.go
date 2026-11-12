package sim

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

var one = []scheduler.Size{{Provider: "p", Name: "m", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 1}}

// one1 limits the test provider to one machine.
var one1 = map[string]scheduler.Limit{"p": {Machines: 1, Source: "test"}}

func opts(cfg scheduler.Config) Options {
	if cfg.IdleStop == 0 {
		cfg.IdleStop = 10 * time.Minute
	}
	return Options{Policy: scheduler.Default{Cfg: cfg}, Sizes: one,
		Boot: map[string]time.Duration{"": time.Minute}, Start: map[string]time.Duration{"": 20 * time.Second}, ColdPenalty: -1}
}

func jobs(js ...Job) Trace { return Trace{Start: DefaultStart, End: 3600, Jobs: js} }

func approx(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func TestSingleJob(t *testing.T) {
	r := Run(jobs(Job{At: 0, Repo: "r", Env: "e", Duration: 300, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib)}), opts(scheduler.Config{}))
	if r.Completed != 1 || r.Machines != 1 || r.WaitP95 != time.Minute || r.OOMs != 0 {
		t.Fatalf("%+v", r)
	}
	// Billed from create until idle stop: 1m boot + 5m run + 10m idle (+ tick).
	approx(t, "machine hours", r.MachineHours, 16.0/60, 0.5/60)
	approx(t, "cost", r.Cost, r.MachineHours, 1e-9)
	approx(t, "slowdown", r.Slowdown, 1, 1e-3)
}

func TestCPUOvercommitSlowsDown(t *testing.T) {
	j := Job{Repo: "r", Env: "e", Duration: 600, CPU: 4, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib)}
	o := opts(scheduler.Config{CPUOvercommit: 2, MemoryOvercommit: 1})
	o.Limits = one1
	r := Run(jobs(j, j), o)
	if r.Machines != 1 || r.Completed != 2 {
		t.Fatalf("%+v", r)
	}
	approx(t, "slowdown", r.Slowdown, 2, 0.01) // 8 cores on 4
}

func TestMemoryBeyondPhysical(t *testing.T) {
	// Requests fit (3G + 3G on 8G); peaks (5G + 5G) do not.
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 1, Memory: Bytes(3 * gib), PeakMemory: Bytes(5 * gib)}
	o := opts(scheduler.Config{})
	o.Limits = one1
	o.MemoryRamp = -1 // peaks for the whole run
	static := Run(jobs(j, j), o)
	if static.OOMs != 4 || static.Completed != 1 || static.Failed != 1 {
		t.Fatalf("static estimate: retries OOM again until they give up: %+v", static)
	}
	o.Estimator = scheduler.NewHistory("")
	learned := Run(jobs(j, j), o)
	if learned.OOMs != 1 || learned.Completed != 2 || learned.WaitMax < 5*time.Minute {
		t.Fatalf("history estimate: one OOM, then the retry waits for room: %+v", learned)
	}
	o.Estimator, o.Swap = nil, true
	sw := Run(jobs(j, j), o)
	if sw.OOMs != 0 || sw.Swapped != 2 {
		t.Fatalf("swap expected: %+v", sw)
	}
	approx(t, "swap slowdown", sw.Slowdown, math.Pow(10.0/8, 2), 0.01)
}

// With the memory ramp a build reaches its peak at its end: two builds
// whose peaks do not fit together run when one is near its end before
// the other one grows, and are OOM killed when they peak together.
func TestMemoryRamp(t *testing.T) {
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 1, Memory: Bytes(3 * gib), PeakMemory: Bytes(5 * gib)}
	o := opts(scheduler.Config{})
	o.Limits = one1
	if r := Run(jobs(j, j), o); r.OOMs == 0 {
		t.Fatalf("both peak at the same time: want an OOM kill, got %+v", r)
	}
	late := j
	late.At, late.Duration, late.PeakMemory = 290, 20, Bytes(2*gib) // starts while the first is near its end
	if r := Run(jobs(j, late), o); r.OOMs != 0 || r.Completed != 2 {
		t.Fatalf("the second build is small: want no OOM, got %+v", r)
	}
}

func TestWarmCacheHits(t *testing.T) {
	j := func(at float64, repo string) Job {
		return Job{At: at, Repo: repo, Env: "e", Duration: 60, CPU: 1, Memory: Bytes(gib), PeakMemory: Bytes(gib)}
	}
	o := opts(scheduler.Config{})
	o.ColdPenalty = 1
	r := Run(jobs(j(0, "a"), j(300, "a"), j(600, "b"), j(900, "a")), o)
	if r.WarmHits != 2 || r.Machines != 1 {
		t.Fatalf("%+v", r)
	}
	approx(t, "slowdown", r.Slowdown, (2+1+2+1)/4.0, 1e-3)
}

func TestQueueTimeoutAndReject(t *testing.T) {
	big := Job{Repo: "r", Env: "e", Duration: 60, CPU: 1, Memory: Bytes(64 * gib), PeakMemory: Bytes(gib)}
	long := Job{Repo: "r", Env: "e", Duration: 3600, CPU: 4, Memory: Bytes(8 * gib), PeakMemory: Bytes(gib)}
	o := opts(scheduler.Config{})
	o.Limits = one1
	o.QueueTimeout = 5 * time.Minute
	r := Run(jobs(big, long, long), o)
	if r.Rejected != 1 || r.TimedOut != 1 || r.Completed != 1 || r.Failed != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestDeterministic(t *testing.T) {
	g := GenOptions{Seed: 42, Days: 2, Repos: Profiles["team"]}
	a, b := Generate(g), Generate(g)
	if !reflect.DeepEqual(a, b) || len(a.Jobs) < 100 {
		t.Fatalf("generator not deterministic (%d / %d jobs)", len(a.Jobs), len(b.Jobs))
	}
	if c := Generate(GenOptions{Seed: 43, Days: 2}); reflect.DeepEqual(a.Jobs, c.Jobs) {
		t.Fatal("seed has no effect")
	}
	sizes := []scheduler.Size{
		{Provider: "p", Name: "s", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.2},
		{Provider: "p", Name: "l", Capacity: scheduler.Resources{CPU: 16, Memory: 32 * gib}, PricePerHour: 0.8},
	}
	for _, variant := range []string{scheduler.VariantDefault, scheduler.VariantClassic} {
		o := func() Options {
			return Options{Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5}},
				Sizes: sizes, Estimator: scheduler.NewHistory("")}
		}
		r1, r2 := Run(a, o()), Run(a, o())
		if !reflect.DeepEqual(r1, r2) {
			t.Fatalf("%s: simulation not deterministic:\n%+v\n%+v", variant, r1, r2)
		}
		if r1.Completed == 0 || r1.Cost <= 0 {
			t.Fatalf("%s: %+v", variant, r1)
		}
	}
}

func TestTraceRoundTrip(t *testing.T) {
	tr := Generate(GenOptions{Seed: 7, Days: 1})
	p := filepath.Join(t.TempDir(), "t.json")
	if err := tr.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(tr.Start) || len(got.Jobs) != len(tr.Jobs) || got.Jobs[3] != tr.Jobs[3] {
		t.Fatalf("round trip differs: %+v vs %+v", got.Jobs[3], tr.Jobs[3])
	}
}

func TestParseCSV(t *testing.T) {
	tr, err := ParseCSV(strings.NewReader(`# start=2026-11-09T08:00:00Z
at,repo,env,duration,cpu,memory,peak_memory
2026-11-09T08:01:00Z,a,e,60,2,2G,1G
30,b,e,90,1,1.5G,512M
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Job{
		{At: 30, Repo: "b", Env: "e", Duration: 90, CPU: 1, Memory: Bytes(3 * gib / 2), PeakMemory: Bytes(512 << 20)},
		{At: 60, Repo: "a", Env: "e", Duration: 60, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib)},
	}
	if !reflect.DeepEqual(tr.Jobs, want) || tr.Start != time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC) {
		t.Fatalf("%+v", tr)
	}
	if _, err := ParseCSV(strings.NewReader("at,repo\n1,a\n")); err == nil {
		t.Fatal("missing columns accepted")
	}
}

// TestSampleTraces checks the cost/time frontier of cost_optimization on
// the sample traces: 0 gives the fastest results, 1 the lowest cost, and
// the default policy at 0 is faster than classic.
func TestSampleTraces(t *testing.T) {
	sizes := []scheduler.Size{
		{Provider: "aws", Name: "c7a.large", Capacity: scheduler.Resources{CPU: 2, Memory: 4 * gib}, PricePerHour: 0.1027},
		{Provider: "aws", Name: "c7a.xlarge", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.2053},
		{Provider: "aws", Name: "c7a.2xlarge", Capacity: scheduler.Resources{CPU: 8, Memory: 16 * gib}, PricePerHour: 0.4106},
		{Provider: "aws", Name: "c7a.4xlarge", Capacity: scheduler.Resources{CPU: 16, Memory: 32 * gib}, PricePerHour: 0.8211},
	}
	tr, err := Load(filepath.Join("testdata", "small-week.json"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(variant string, r float64) Report {
		return Run(tr, Options{Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5,
			CostOptimization: r, QueueTimeout: 10 * time.Minute}}, Sizes: sizes, Estimator: scheduler.NewHistory("")})
	}
	fast, cheap, classic := run(scheduler.VariantDefault, 0), run(scheduler.VariantDefault, 1), run(scheduler.VariantClassic, 0)
	if cheap.Cost >= fast.Cost || fast.TurnP95 > cheap.TurnP95 || fast.Completed != fast.Jobs || cheap.Completed != cheap.Jobs {
		t.Errorf("frontier: r=0 cost %.2f turn95 %v done %d, r=1 cost %.2f turn95 %v done %d",
			fast.Cost, fast.TurnP95, fast.Completed, cheap.Cost, cheap.TurnP95, cheap.Completed)
	}
	if fast.TurnP95 > classic.TurnP95 || fast.Cost > classic.Cost {
		t.Errorf("r=0 cost %.2f turn95 %v, classic cost %.2f turn95 %v", fast.Cost, fast.TurnP95, classic.Cost, classic.TurnP95)
	}
}

func diskOpts(cfg scheduler.Config, disk int64, resize bool) Options {
	o := opts(cfg)
	o.Sizes = []scheduler.Size{{Provider: "p", Name: "m", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib, Disk: disk}, PricePerHour: 1}}
	o.Resize, o.Estimator = resize, scheduler.NewHistory("")
	return o
}

// Two repositories whose slots do not fit the disk together: the model
// prunes (or resizes when the provider can) instead of letting the worker
// evict at random.
func TestSimDiskFull(t *testing.T) {
	var js []Job
	for i := 0; i < 12; i++ {
		repo := []string{"a", "b"}[i%2]
		js = append(js, Job{At: float64(i * 400), Repo: repo, Env: "e", Duration: 120, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib),
			SlotBytes: Bytes(30 * gib), SlotGrowth: Bytes(gib)})
	}
	tr := Trace{Start: DefaultStart, End: 6000, Jobs: js}
	r := Run(tr, diskOpts(scheduler.Config{}, 50*gib, false))
	if r.Completed != len(js) || r.Prunes == 0 {
		t.Fatalf("no resize: want prunes, got %+v", r)
	}
	r = Run(tr, diskOpts(scheduler.Config{}, 50*gib, true))
	if r.Completed != len(js) || r.Resizes == 0 || r.Evictions > 1 || r.WarmHits < 8 {
		t.Fatalf("resize: want a resize and warm slots, got %+v", r)
	}
}

// A machine expires; its slot snapshot is restored on the next machine
// instead of building cold.
func TestSimRestoreAfterExpiry(t *testing.T) {
	js := []Job{
		{At: 0, Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), SlotBytes: Bytes(5 * gib)},
		{At: 700, Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), SlotBytes: Bytes(5 * gib)},
		{At: 4000, Repo: "r", Env: "e", Duration: 600, CPU: 2, Memory: Bytes(2 * gib), PeakMemory: Bytes(gib), SlotBytes: Bytes(5 * gib)},
	}
	o := diskOpts(scheduler.Config{}, 0, false)
	o.MaxLifetime, o.ColdPenalty = time.Hour, 1
	r := Run(Trace{Start: DefaultStart, End: 6000, Jobs: js}, o)
	if r.Completed != 3 || r.Restores != 1 {
		t.Fatalf("want one restore, got %+v", r)
	}
}

// The cheaper provider has no capacity: work goes to the other one.
func TestSimProviderOutage(t *testing.T) {
	o := opts(scheduler.Config{})
	o.Sizes = []scheduler.Size{
		{Provider: "a", Name: "m", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.1},
		{Provider: "b", Name: "m", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.5},
	}
	o.Down = []Outage{{Provider: "a", From: 0, To: 1e9}}
	r := Run(jobs(Job{At: 0, Repo: "r", Env: "e", Duration: 60, CPU: 2, Memory: Bytes(gib), PeakMemory: Bytes(gib)}), o)
	if r.Completed != 1 || r.Actions["create-failed:no capacity"] != 0 {
		t.Fatalf("want a create on b only, got %+v", r)
	}
}

// Production 2026-11-12 13:23: three queued sessions, slow boots, eight
// creates (one per plan tick per overdue request). The policy creates at
// most one machine per request, fewer when they fit together (an x4 takes
// two of them).
func TestRegressionSlowBootNoOverProvisioning(t *testing.T) {
	x := []scheduler.Size{
		{Provider: "aws", Name: "x4", Capacity: scheduler.Resources{CPU: 4, Memory: 7 * gib}, PricePerHour: 0.23},
		{Provider: "aws", Name: "x8", Capacity: scheduler.Resources{CPU: 8, Memory: 15 * gib}, PricePerHour: 0.46},
	}
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 2, Memory: Bytes(3 * gib), PeakMemory: Bytes(2 * gib)}
	for _, r := range []float64{0, 0.2, 0.5} {
		o := Options{Policy: scheduler.Default{Cfg: scheduler.Config{CostOptimization: r, CPUOvercommit: 2, MemoryOvercommit: 1,
			QueueTimeout: 10 * time.Minute, IdleStop: time.Minute}}, Sizes: x,
			Boot: map[string]time.Duration{"": 70 * time.Second}, Start: map[string]time.Duration{"": 15 * time.Second},
			BootOverrun: 2 * time.Minute, Reconcile: 2 * time.Second, ColdPenalty: -1, QueueTimeout: 10 * time.Minute}
		a, b, c := j, j, j
		b.At, c.At = 1, 2
		rep := Run(jobs(a, b, c), o)
		if rep.Completed != 3 || rep.Machines > 2 {
			t.Fatalf("r=%v: want 3 completed on at most 2 machines, got %d machines: %+v", r, rep.Machines, rep)
		}
		t.Logf("r=%v: %d machines, wait max %s", r, rep.Machines, rep.WaitMax)
	}
}
