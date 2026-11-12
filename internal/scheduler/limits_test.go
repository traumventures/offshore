package scheduler

import (
	"testing"
	"time"
)

// Hard limits: a provider's vCPU quota, a disabled provider and the spend
// circuit breaker forbid creates and starts whichever step proposed them.
func TestHardLimits(t *testing.T) {
	sizes := []Size{{Provider: "a", Name: "s", Capacity: res(8, 16), PricePerHour: 0.4}, {Provider: "b", Name: "s", Capacity: res(8, 16), PricePerHour: 0.5}}
	run := busy("run", res(8, 16), res(8, 16), 1)
	run.Provider, run.Size = "a", "s"
	stopped := machine("st", res(8, 16), Resources{})
	stopped.Provider, stopped.Size, stopped.State = "a", "s", Stopped
	f := Pool{Now: now, Sizes: sizes, Machines: []Machine{run, stopped}, Queue: queue(task(4, 8))}
	f.Queue[0].Since = now.Add(-time.Hour)
	for _, v := range []string{""} {
		for _, c := range []struct {
			name  string
			cfg   Config
			limit map[string]Limit
			want  string // provider of the scale-out, "" = none
		}{
			{"no limit", Config{}, nil, "a"},
			{"vCPU quota of a used up", Config{}, map[string]Limit{"a": {CPU: 8, Source: "quota"}}, "b"},
			{"a disabled", Config{DisabledProviders: map[string]bool{"a": true}}, nil, "b"},
			{"spend breaker", Config{MaxSpendPerHour: 0.6}, nil, ""},
		} {
			c.cfg.CPUOvercommit, c.cfg.MemoryOvercommit = 1, 1
			f.ProviderLimit = c.limit
			got := ""
			for _, a := range (Default{Cfg: c.cfg}).Plan(f) {
				switch a.Kind {
				case Create:
					got = a.Size.Provider
				case Start:
					got = "a"
				}
			}
			if got != c.want {
				t.Fatalf("%s/%s: scale-out on %q, want %q", v, c.name, got, c.want)
			}
		}
	}
}
