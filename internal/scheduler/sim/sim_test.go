package sim

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/scheduler"
)

var one = []scheduler.Size{{Provider: "p", Name: "m", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 1}}

func opts(cfg scheduler.Config) Options {
	if cfg.MaxMachines == 0 {
		cfg.MaxMachines = 4
	}
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
	r := Run(jobs(j, j), opts(scheduler.Config{CPUOvercommit: 2, MemoryOvercommit: 1, MaxMachines: 1}))
	if r.Machines != 1 || r.Completed != 2 {
		t.Fatalf("%+v", r)
	}
	approx(t, "slowdown", r.Slowdown, 2, 0.01) // 8 cores on 4
}

func TestMemoryBeyondPhysical(t *testing.T) {
	// Requests fit (3G + 3G on 8G); peaks (5G + 5G) do not.
	j := Job{Repo: "r", Env: "e", Duration: 300, CPU: 1, Memory: Bytes(3 * gib), PeakMemory: Bytes(5 * gib)}
	o := opts(scheduler.Config{MaxMachines: 1})
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
	o := opts(scheduler.Config{MaxMachines: 1})
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
			return Options{Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1,
				MaxMachines: 8, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5}},
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

// TestSampleTraces checks the claim of docs/SCHEDULER.md on the sample
// traces: the default policy is cheaper than classic at equal or better p95
// wait.
func TestSampleTraces(t *testing.T) {
	sizes := []scheduler.Size{
		{Provider: "aws", Name: "c7a.large", Capacity: scheduler.Resources{CPU: 2, Memory: 4 * gib}, PricePerHour: 0.1027},
		{Provider: "aws", Name: "c7a.xlarge", Capacity: scheduler.Resources{CPU: 4, Memory: 8 * gib}, PricePerHour: 0.2053},
		{Provider: "aws", Name: "c7a.2xlarge", Capacity: scheduler.Resources{CPU: 8, Memory: 16 * gib}, PricePerHour: 0.4106},
		{Provider: "aws", Name: "c7a.4xlarge", Capacity: scheduler.Resources{CPU: 16, Memory: 32 * gib}, PricePerHour: 0.8211},
	}
	for _, name := range []string{"team-week.json", "small-week.json"} {
		tr, err := Load(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		run := func(variant string) Report {
			return Run(tr, Options{Policy: scheduler.Default{Cfg: scheduler.Config{Variant: variant, CPUOvercommit: 2, MemoryOvercommit: 1,
				MaxMachines: 16, IdleStop: 20 * time.Minute, ExpiryMargin: 15 * time.Minute, ConsolidateBelow: 0.5}}, Sizes: sizes})
		}
		d, c := run(scheduler.VariantDefault), run(scheduler.VariantClassic)
		if d.Cost >= c.Cost || d.WaitP95 > c.WaitP95 || d.Completed < c.Completed {
			t.Errorf("%s: default cost %.2f p95 %v done %d, classic cost %.2f p95 %v done %d",
				name, d.Cost, d.WaitP95, d.Completed, c.Cost, c.WaitP95, c.Completed)
		}
	}
}
