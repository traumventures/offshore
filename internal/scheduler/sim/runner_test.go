package sim

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/traumventures/offshore/internal/scheduler"
)

// determinismSpecs is a batch over traces, pools and policy variants
// with fresh estimators (specs are built anew for every batch).
func determinismSpecs(t *testing.T) []Spec {
	pools := map[string]EvalPool{}
	for _, p := range EvalPools() {
		pools[p.Name] = p
	}
	type tc struct {
		tr     Trace
		pool   string
		down   []Outage
		resize bool
	}
	var cases []tc
	nov10, err := Load(filepath.Join("testdata", "prod-nov10.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, tc{tr: withSlots(nov10), pool: "prod"})
	for _, seed := range []uint64{1} {
		for _, sc := range Scenarios(seed) {
			switch {
			case sc.Tiers:
				cases = append(cases, tc{tr: sc.Trace, pool: "tiers", resize: true})
			default:
				cases = append(cases, tc{tr: withSlots(sc.Trace), pool: "prod", down: sc.Down})
			}
		}
	}
	var specs []Spec
	for _, c := range cases {
		p := pools[c.pool]
		for _, v := range []evalRun{{"v2", 0.2}, {"v2", 1}} {
			cfg := p.Cfg
			cfg.CostOptimization = v.r
			var est scheduler.Estimator = scheduler.Static{}
			if p.History {
				est = scheduler.NewHistory("")
			}
			specs = append(specs, Spec{Trace: c.tr, Options: Options{Name: c.pool + " " + v.String(), Policy: scheduler.Default{Cfg: cfg},
				Sizes: p.Sizes, Estimator: est, Boot: p.Boot, Start: p.Start, Down: c.down, Resize: c.resize, QueueTimeout: cfg.QueueTimeout}})
		}
	}
	return specs
}

// TestRunAllDeterministic: a batch gives identical reports serially and
// in parallel, and again when repeated.
func TestRunAllDeterministic(t *testing.T) {
	serial := RunAll(determinismSpecs(t), 1)
	par := RunAll(determinismSpecs(t), 4)
	again := RunAll(determinismSpecs(t), 8)
	for i := range serial {
		if !reflect.DeepEqual(serial[i], par[i]) {
			t.Errorf("run %d (%s): serial and parallel reports differ:\n%+v\n%+v", i, serial[i].Name, serial[i], par[i])
		}
		if !reflect.DeepEqual(par[i], again[i]) {
			t.Errorf("run %d (%s): repeated reports differ:\n%+v\n%+v", i, par[i].Name, par[i], again[i])
		}
	}
}

// TestCached: the binary cache returns the parsed trace and parses again
// when the file changes.
func TestCached(t *testing.T) {
	t.Setenv("OFFSHORE_SIM_CACHE", t.TempDir())
	src := filepath.Join(t.TempDir(), "trace.json")
	tr := Generate(GenOptions{Seed: 3, Days: 1, Repos: Profiles["small"], Start: DefaultStart})
	if err := tr.Save(src); err != nil {
		t.Fatal(err)
	}
	parses := 0
	load := func() (Trace, error) { parses++; return Load(src) }
	first, err := Cached(src, "trace", load)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Cached(src, "trace", load)
	if err != nil {
		t.Fatal(err)
	}
	if parses != 1 {
		t.Fatalf("parsed %d times, want 1 (cache hit)", parses)
	}
	if !first.Start.Equal(second.Start) || !reflect.DeepEqual(first.Jobs, second.Jobs) || first.End != second.End {
		t.Fatal("cached trace differs from the parsed one")
	}
	if _, err := Cached(src, "other options", load); err != nil || parses != 2 {
		t.Fatalf("other key: parses %d, err %v; want a new parse", parses, err)
	}
	tr.Jobs = tr.Jobs[:1]
	if err := tr.Save(src); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(src, first.Start, first.Start) // a different modification time
	third, err := Cached(src, "trace", load)
	if err != nil || len(third.Jobs) != 1 || parses != 3 {
		t.Fatalf("changed file: %d jobs, %d parses, err %v", len(third.Jobs), parses, err)
	}
}
