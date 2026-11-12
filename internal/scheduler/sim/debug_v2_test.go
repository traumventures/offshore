package sim

import (
	"encoding/json"
	"fmt"

	"github.com/traumventures/offshore/internal/scheduler"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDebugV2(t *testing.T) {
	name := os.Getenv("DBG_CASE")
	if name == "" {
		t.Skip()
	}
	r, _ := strconv.ParseFloat(os.Getenv("DBG_R"), 64)
	for _, c := range gateCases(t) {
		if c.name != name {
			continue
		}
		for _, v := range strings.Split(os.Getenv("DBG_V"), ",") {
			sp := c.pool.Spec(v, c.trace, v, r, c.down, c.resize)
			if w := os.Getenv("DBG_FROM"); w != "" {
				from, _ := strconv.ParseFloat(w, 64)
				sp.Options.Policy = dbgPolicy{Policy: sp.Options.Policy, t: t, from: sp.Trace.Start.Add(time.Duration(from) * time.Second), n: new(int)}
			}
			n := 0
			sp.Options.Debug = func(j Job, w time.Duration) {
				if w > 2*time.Minute && n < 15 {
					n++
					t.Logf("%s job at %.0f repo %s cpu %.1f wait %s", v, j.At, j.Repo, j.CPU, w)
				}
			}
			rep := Run(sp.Trace, sp.Options)
			b, _ := json.Marshal(rep.Actions)
			t.Logf("%s cold %.2f warm %.2f restores %d slow %.2f mh %.1f idle %.1f vol %.2f maxrun %d p95 %s cost %.2f failed %d timedout %d rejected %d ooms %d actions %s created %v", v, rep.ColdRate, rep.WarmRate, rep.Restores, rep.Slowdown, rep.MachineHours, rep.IdleHours, rep.VolumeCost, rep.MaxRunning, rep.TurnP95, rep.Cost, rep.Failed, rep.TimedOut, rep.Rejected, rep.OOMs, b, rep.Created)
		}
	}
}

type dbgPolicy struct {
	scheduler.Policy
	t    *testing.T
	from time.Time
	n    *int
}

func (d dbgPolicy) Place(f scheduler.Pool, t scheduler.Task) scheduler.Placement {
	pl := d.Policy.Place(f, t)
	if f.Now.After(d.from) && *d.n < 25 {
		*d.n++
		var b strings.Builder
		for _, m := range f.Machines {
			fmt.Fprintf(&b, " [%s %d res %.1f sess %d]", m.ID, m.State, m.Reserved.CPU, m.Sessions)
		}
		d.t.Logf("%s place %s kind %d %s machines:%s", f.Now.Sub(d.from), t.ID, pl.Kind, pl.Reason, b.String())
		if pl.Decision != nil {
			for i, o := range pl.Decision.Options {
				if i > 4 {
					break
				}
				d.t.Logf("    %s %s %s/%s slot %s wait %.0f t %.0f cost %.4f score %.4f %s [%s]", o.Kind, o.Machine, o.Provider, o.Size, o.Slot, o.WaitSeconds, o.TimeSeconds, o.Cost, o.Score, o.Rejected, o.Reason)
			}
		}
	}
	return pl
}

func (d dbgPolicy) Plan(f scheduler.Pool) []scheduler.Action {
	acts := d.Policy.Plan(f)
	if os.Getenv("DBG_CREATES") != "" {
		for _, a := range acts {
			if a.Kind != scheduler.Create {
				continue
			}
			var b strings.Builder
			for _, m := range f.Machines {
				fmt.Fprintf(&b, " [%s %d res %.1f sess %d]", m.ID, m.State, m.Reserved.CPU, m.Sessions)
			}
			d.t.Logf("%s CREATE %s queue %d machines:%s", f.Now.Format("15:04:05"), a.Reason, len(f.Queue), b.String())
			if a.Decision != nil {
				for i, o := range a.Decision.Options {
					if i > 5 {
						break
					}
					d.t.Logf("    %s %s %s slot %s wait %.0f t %.0f cost %.4f score %.4f %s [%s]", o.Kind, o.Machine, o.Size, o.Slot, o.WaitSeconds, o.TimeSeconds, o.Cost, o.Score, o.Rejected, o.Reason)
				}
			}
		}
	}
	if f.Now.After(d.from) && *d.n < 25 {
		for _, a := range acts {
			d.t.Logf("%s plan %s %s %s", f.Now.Sub(d.from), a.Kind, a.Machine, a.Reason)
		}
	}
	return acts
}
