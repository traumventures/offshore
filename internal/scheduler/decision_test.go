package scheduler

import (
	"strings"
	"testing"
	"time"
)

// The decision of a placement lists every option: waiting for the busy
// warm machine and running elsewhere after moving/restoring the slot or
// cold, with times, costs and scores; the chosen one first and with the
// best score.
func TestPlacementDecisionListsOptions(t *testing.T) {
	warm := busy("warm", res(8, 16), res(8, 16), 1)
	warm.Size, warm.WarmRepos = "m", []string{"r"}
	warm.SlotBytes = map[string]int64{"r": 2 * gib}
	warm.Releases = []Release{{At: now.Add(8 * time.Minute), Res: res(8, 16)}}
	idle := machine("idle", res(8, 16), Resources{})
	idle.Size, idle.IdleSince = "m", now.Add(-time.Minute)
	f := Pool{Now: now, Sizes: priced, Machines: []Machine{warm, idle},
		Snapshots: map[string][]Snapshot{"r": {{Repo: "r", Provider: "p", Store: "p", Bytes: 2 * gib, At: now.Add(-time.Hour)}}}}
	tk := profiled(5*time.Minute, 10*time.Minute)
	for _, variant := range []string{"", "costmodel"} {
		pol := Default{Cfg: Config{CostOptimization: 0.5, CPUOvercommit: 1, MemoryOvercommit: 1, Variant: variant}}
		pl := pol.Place(f, tk)
		d := pl.Decision
		if d == nil || d.Chosen != 0 || !d.Options[0].Chosen {
			t.Fatalf("%q: decision %+v", variant, d)
		}
		kinds := map[string]bool{}
		for i, o := range d.Options {
			kinds[o.Kind+"/"+o.Slot], kinds[o.Kind] = true, true
			if o.Rejected == "" && i > 0 && o.Score < d.Options[0].Score-1e-9 {
				t.Fatalf("%q: option %d scores better than the chosen one: %+v", variant, i, d.Options)
			}
		}
		for _, want := range []string{"wait_machine", "assign/restore", "assign/cold"} {
			if !kinds[want] {
				t.Fatalf("%q: no %s option in %+v", variant, want, d.Options)
			}
		}
		c, _ := d.ChosenOption()
		if (pl.Kind == Assign) != (c.Kind == OptAssign) || c.TimeSeconds <= 0 {
			t.Fatalf("%q: chosen %+v for placement %+v", variant, c, pl)
		}
		if (variant == "") != (d.Policy == "options") {
			t.Fatalf("%q: policy %s", variant, d.Policy)
		}
	}
	// A restore records its predicted preparation time.
	pl := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, tk)
	for _, o := range pl.Decision.Options {
		if o.Slot == "restore" && o.PrepSeconds <= 0 {
			t.Fatalf("restore without prep time: %+v", o)
		}
	}
}

// Rejected candidates carry their reason.
func TestDecisionRejections(t *testing.T) {
	small := busy("small", res(2, 4), res(2, 4), 1)
	small.Size = "s"
	f := Pool{Now: now, Sizes: append([]Size{{Provider: "p", Name: "s", Capacity: res(2, 4), PricePerHour: 0.1}}, priced...),
		Machines: []Machine{small, machine("big", res(8, 16), Resources{})}}
	pl := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Place(f, task(4, 8))
	found := false
	for _, o := range pl.Decision.Options {
		if o.Machine == "small" && strings.Contains(o.Rejected, "smaller") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rejection of the small machine: %+v", pl.Decision.Options)
	}
	if pl := (Default{Cfg: Config{}}).Place(Pool{Now: now, Sizes: priced}, task(64, 512)); pl.Kind != Reject || pl.Decision == nil {
		t.Fatalf("reject without decision: %+v", pl)
	}
}

// A create carries the scale-out decision with the waiting option and
// the sizes it compared.
func TestScaleOutDecision(t *testing.T) {
	sizes := []Size{
		{Provider: "a", Name: "m", Capacity: res(8, 16), PricePerHour: 0.4},
		{Provider: "b", Name: "m", Capacity: res(8, 16), PricePerHour: 0.5},
	}
	tk := profiled(5*time.Minute, 10*time.Minute)
	tk.Since = now.Add(-10 * time.Minute)
	f := Pool{Now: now, Sizes: sizes, Queue: []Task{tk},
		Providers: map[string]ProviderState{"b": {Down: now.Add(time.Hour)}}}
	acts := Default{Cfg: Config{CPUOvercommit: 1, MemoryOvercommit: 1}}.Plan(f)
	var create *Action
	for i := range acts {
		if acts[i].Kind == Create {
			create = &acts[i]
		}
	}
	if create == nil || create.Decision == nil {
		t.Fatalf("no create with decision: %+v", acts)
	}
	c, ok := create.Decision.ChosenOption()
	if !ok || c.Kind != OptCreate || c.Provider != "a" || c.Tasks != 1 || c.LatencySeconds <= 0 {
		t.Fatalf("chosen %+v", c)
	}
	down := false
	for _, o := range create.Decision.Options {
		if o.Provider == "b" && o.Rejected == RejProviderDown {
			down = true
		}
	}
	if !down {
		t.Fatalf("provider b not rejected as down: %+v", create.Decision.Options)
	}
}
