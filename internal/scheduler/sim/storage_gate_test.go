package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// storageCases are the storage gate's traces (docs/design-storage-v2.md):
// the sample weeks and production days on the production pool (200 GB
// gp3 disks the scheduler may grow), the production days on 100 GB disks
// (disk-tight), the adversarial disk scenario on local tiers and a
// five-day trace of growing Rust slots.
func storageCases(t testing.TB) []evalCase {
	pools := map[string]EvalPool{}
	for _, p := range EvalPools() {
		p.History = true
		pools[p.Name] = p
	}
	prod := pools["prod"]
	tight := prod
	tight.Name = "prod100"
	tight.Sizes = append(tight.Sizes[:0:0], prod.Sizes...)
	for i := range tight.Sizes {
		tight.Sizes[i].Capacity.Disk = 100 * gib
	}
	var cs []evalCase
	for _, name := range []string{"small-week", "team-week", "prod-nov11-day", "prod-nov12"} {
		tr, err := Load(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, evalCase{name: name + "/prod", pool: prod, trace: withSlots(tr), resize: true})
		if strings.HasPrefix(name, "prod-") {
			cs = append(cs, evalCase{name: name + "/prod100", pool: tight, trace: withSlots(tr), resize: true})
		}
	}
	for _, sc := range Scenarios(1) {
		if sc.Name == "disk" {
			cs = append(cs, evalCase{name: "disk/tiers", pool: pools["tiers"], trace: sc.Trace, resize: true})
		}
	}
	cs = append(cs, evalCase{name: "growth-5d/prod", pool: prod, trace: GrowthTrace(1, 5), resize: true})
	return cs
}

// TestStorageGate runs the storage gate: OFFSHORE_SGATE=label (e.g. v2),
// OFFSHORE_SGATE_JSON writes the reports,
// OFFSHORE_SGATE_BASE adds those of an earlier run (the scheduler and
// worker before storage v2 are no longer in the tree:
// testdata/storage-gate-base.json) as the first variant "base",
// OFFSHORE_SGATE_CASE filters cases, OFFSHORE_SGATE_OUT writes the table.
// Primary metrics in order: warm-start rate, ENOSPC (must be 0), disk
// waits (must be 0), then cost and p95 turnaround.
func TestStorageGate(t *testing.T) {
	v := os.Getenv("OFFSHORE_SGATE")
	if v == "" {
		t.Skip("set OFFSHORE_SGATE=<label> to run the storage gate")
	}
	base := map[string]Report{}
	if p := os.Getenv("OFFSHORE_SGATE_BASE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &base); err != nil {
			t.Fatal(err)
		}
	}
	rs := []float64{0.2, 0.8}
	filter := os.Getenv("OFFSHORE_SGATE_CASE")
	var cases []evalCase
	for _, c := range storageCases(t) {
		if filter == "" || strings.Contains(c.name, filter) {
			cases = append(cases, c)
		}
	}
	var specs []Spec
	var keys []string
	for _, c := range cases {
		for _, r := range rs {
			sp := c.pool.Spec(fmt.Sprintf("%s r=%.1f", v, r), c.trace, "v2", r, c.down, c.resize)
			specs = append(specs, sp)
			keys = append(keys, fmt.Sprintf("%s|%.1f", c.name, r))
		}
	}
	start := time.Now()
	ran := RunAll(specs, 0)
	out := map[string]Report{}
	for i, k := range keys {
		out[k] = ran[i]
	}
	if p := os.Getenv("OFFSHORE_SGATE_JSON"); p != "" {
		b, _ := json.Marshal(out)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	fmt.Fprintln(&b, "| case | r | worker | warm starts | ENOSPC | disk waits | disk wait | cost $ | turn p95 | failed | max disk | demoted | evicted | store lag p95 |")
	fmt.Fprintln(&b, "|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	row := func(name string, r float64, label string, rep Report) {
		fmt.Fprintf(&b, "| %s | %.1f | %s | %.1f%% | %d | %d | %s | %.2f | %s | %d | %.0f%% | %d | %d | %s |\n", name, r, label,
			100*rep.WarmStartRate, rep.ENOSPC, rep.DiskWaits, rep.DiskWaitTime.Round(time.Second), rep.Cost,
			rep.TurnP95.Round(time.Second), rep.Failed, 100*rep.MaxDiskShare, rep.Demoted, rep.Evictions, rep.StoreLagP95.Round(time.Second))
	}
	worse := 0
	for _, k := range keys {
		name, rs, _ := strings.Cut(k, "|")
		var r float64
		fmt.Sscan(rs, &r)
		if br, ok := base[k]; ok {
			row(name, r, "base", br)
			nr := out[k]
			if nr.WarmStartRate < br.WarmStartRate-0.005 || nr.Cost > br.Cost*1.01 || nr.TurnP95 > br.TurnP95+br.TurnP95/50 {
				worse++
				t.Logf("worse than base: %s (warm %.3f/%.3f cost %.2f/%.2f p95 %s/%s)", k, nr.WarmStartRate, br.WarmStartRate,
					nr.Cost, br.Cost, nr.TurnP95, br.TurnP95)
			}
		}
		row(name, r, v, out[k])
		if out[k].ENOSPC > 0 || out[k].DiskWaits > 0 {
			t.Errorf("%s: %d ENOSPC, %d disk waits", k, out[k].ENOSPC, out[k].DiskWaits)
		}
	}
	if p := os.Getenv("OFFSHORE_SGATE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d simulations in %s, %d cases worse than base\n%s", len(specs), time.Since(start).Round(time.Second), worse, b.String())
}
