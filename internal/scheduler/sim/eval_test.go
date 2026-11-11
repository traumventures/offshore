package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// evalCase is one trace on one pool of the evaluation matrix.
type evalCase struct {
	name   string
	pool   EvalPool
	trace  Trace
	down   []Outage
	resize bool
}

// evalCases are the evaluation traces: the sample traces, the production
// exports (nov10: the old control plane's day, nov11: today's with the
// waiting regression) and the adversarial scenarios, each on the pools it
// stresses.
func evalCases(t testing.TB) []evalCase {
	pools := map[string]EvalPool{}
	for _, p := range EvalPools() {
		pools[p.Name] = p
	}
	var cs []evalCase
	for _, name := range []string{"team-week", "small-week", "prod-flap", "prod-nov10", "prod-nov11"} {
		tr, err := Load(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"c7a", "prod"} {
			tp := tr
			if p == "prod" {
				tp = withSlots(tr)
			}
			cs = append(cs, evalCase{name: name + "/" + p, pool: pools[p], trace: tp})
		}
	}
	for _, sc := range Scenarios(1) {
		if sc.Tiers {
			cs = append(cs, evalCase{name: sc.Name + "/tiers", pool: pools["tiers"], trace: sc.Trace, resize: true})
			continue
		}
		for _, p := range []string{"c7a", "prod"} {
			tp := sc.Trace
			if p == "prod" {
				tp = withSlots(tp)
			}
			cs = append(cs, evalCase{name: sc.Name + "/" + p, pool: pools[p], trace: tp, down: sc.Down})
		}
	}
	return cs
}

// withSlots gives jobs without cache data the slot of a production app
// worktree (8G after a cold build, +200M per warm build, 3G working set,
// like the snapshots and page-ins of November 2026), so restores from
// the store (written back after every build) are options on the
// production pool.
func withSlots(tr Trace) Trace {
	out := tr
	out.Jobs = append([]Job(nil), tr.Jobs...)
	for i := range out.Jobs {
		j := &out.Jobs[i]
		if j.SlotBytes == 0 {
			j.SlotBytes, j.SlotGrowth, j.WorkingSet = Bytes(8*gib), Bytes(200<<20), Bytes(3*gib)
		}
	}
	return out
}

type evalRun struct {
	variant string
	r       float64
}

func (v evalRun) String() string {
	if v.variant == "binpack" || v.variant == "classic" {
		return v.variant
	}
	return fmt.Sprintf("%s r=%.1f", v.variant, v.r)
}

// TestEvalTable prints the policy evaluation (markdown) when
// OFFSHORE_EVAL is set: OFFSHORE_EVAL=default,binpack (variants),
// OFFSHORE_EVAL_R=0,0.5,1 (cost optimization values), OFFSHORE_EVAL_CASE
// (substring filter of case names).
func TestEvalTable(t *testing.T) {
	vs := os.Getenv("OFFSHORE_EVAL")
	if vs == "" {
		t.Skip("set OFFSHORE_EVAL=variant,... to print the evaluation table")
	}
	rs := []float64{0, 0.2, 0.5, 0.8, 1}
	if s := os.Getenv("OFFSHORE_EVAL_R"); s != "" {
		rs = nil
		for _, x := range strings.Split(s, ",") {
			v, err := strconv.ParseFloat(x, 64)
			if err != nil {
				t.Fatal(err)
			}
			rs = append(rs, v)
		}
	}
	var runs []evalRun
	for _, v := range strings.Split(vs, ",") {
		if v == "binpack" || v == "classic" {
			runs = append(runs, evalRun{v, 0})
			continue
		}
		for _, r := range rs {
			runs = append(runs, evalRun{v, r})
		}
	}
	filter := os.Getenv("OFFSHORE_EVAL_CASE")
	var cases []evalCase
	for _, c := range evalCases(t) {
		if filter == "" || strings.Contains(c.name, filter) {
			cases = append(cases, c)
		}
	}
	reports := make([][]Report, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cases {
		reports[i] = make([]Report, len(runs))
		for k, v := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				reports[i][k] = c.pool.Run(v.String(), c.trace, v.variant, v.r, c.down, c.resize)
			}()
		}
	}
	wg.Wait()
	var b strings.Builder
	fmt.Fprintln(&b, "| case | policy | cost $ | idle h | turn p50 | turn p95 | wait p95 | cold | flaps | late | regret p95 | failed |")
	fmt.Fprintln(&b, "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for i, c := range cases {
		for _, r := range reports[i] {
			fmt.Fprintf(&b, "| %s | %s | %.2f | %.1f | %s | %s | %s | %.0f%% | %d | %d | %s | %d |\n", c.name, r.Name, r.Cost, r.IdleHours,
				r.TurnP50.Round(time.Second), r.TurnP95.Round(time.Second), r.WaitP95.Round(time.Second), 100*r.ColdRate, r.Flaps, r.Late,
				r.RegretP95.Round(time.Second), r.Failed)
		}
	}
	if out := os.Getenv("OFFSHORE_EVAL_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("\n" + b.String())

}

// TestEvalOne prints the full report of one case (OFFSHORE_EVAL_ONE=case,
// OFFSHORE_EVAL=variant, OFFSHORE_EVAL_R=r).
func TestEvalOne(t *testing.T) {
	name := os.Getenv("OFFSHORE_EVAL_ONE")
	if name == "" {
		t.Skip("set OFFSHORE_EVAL_ONE")
	}
	r, _ := strconv.ParseFloat(os.Getenv("OFFSHORE_EVAL_R"), 64)
	for _, c := range evalCases(t) {
		if c.name != name {
			continue
		}
		for _, v := range strings.Split(os.Getenv("OFFSHORE_EVAL"), ",") {
			rep := c.pool.Run(v, c.trace, v, r, c.down, c.resize)
			b, _ := json.MarshalIndent(rep, "", " ")
			t.Logf("%s\n%s", v, b)
		}
	}
}
