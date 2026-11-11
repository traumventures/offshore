package orchestrator

import (
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
)

// uptimeHistory is how far back a restarted orchestrator replays machine
// events to recover uptime (cost so far today must survive restarts).
const uptimeHistory = 26 * time.Hour

// replayUptime rebuilds machines and their uptime spans from machine
// events (create, start, ready, stop, destroy, state). gone machines were
// destroyed (or vanished); the others were last seen and get merged into
// the next provider listing. Open spans of gone machines end at their
// last event.
func replayUptime(evs []events.Event) (gone []*Machine, seen map[string]*Machine) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
	all := map[string]*Machine{}
	last := map[string]time.Time{}
	// Only machines that demonstrably existed count: a create request
	// alone (e.g. one of a failed-create storm) is no machine.
	alive := map[string]bool{}
	for _, ev := range evs {
		up, known := upEvent(ev)
		if !known || ev.Machine == "" || ev.Time.IsZero() {
			continue
		}
		m := all[ev.Machine]
		if m == nil {
			m = &Machine{Machine: provider.Machine{ID: ev.Machine, Labels: map[string]string{}, CreatedAt: ev.Time}, observed: true}
			all[ev.Machine] = m
		}
		if ev.Provider != "" {
			m.Provider = ev.Provider
		}
		if ev.Size != "" {
			m.Labels[provider.LabelSize] = ev.Size
		}
		if ev.Env != "" {
			m.EnvHash = ev.Env
		}
		if ev.Price > 0 {
			m.price = ev.Price
		}
		open := len(m.Uptime) > 0 && m.Uptime[len(m.Uptime)-1].End.IsZero()
		switch {
		case up && !open:
			m.Uptime = append(m.Uptime, Span{Start: ev.Time})
		case !up && open:
			m.Uptime[len(m.Uptime)-1].End = ev.Time
		}
		m.State = provider.Stopped
		if up {
			m.State = provider.Running
		}
		if ev.Type == events.MachineDestroy || (ev.Type == events.MachineState && ev.State == "gone") {
			m.State = provider.Failed
		}
		last[ev.Machine] = ev.Time
		switch ev.Type {
		case events.MachineReady, events.MachineSample, events.MachineStart, events.MachineStop:
			alive[ev.Machine] = true
		case events.MachineState:
			alive[ev.Machine] = alive[ev.Machine] || ev.State == string(provider.Running)
		}
	}
	seen = map[string]*Machine{}
	for id, m := range all {
		if len(m.Uptime) == 0 || !alive[id] {
			continue
		}
		if m.State != provider.Failed {
			m.listed = last[id] // last sign of life (see mergeUptimeLocked)
			seen[id] = m
			continue
		}
		if sp := &m.Uptime[len(m.Uptime)-1]; sp.End.IsZero() {
			sp.End = last[id]
		}
		m.Removed = last[id]
		gone = append(gone, m)
	}
	return gone, seen
}

// upEvent reports whether a machine event means the machine is up
// (costs money) and whether the event says anything about it.
func upEvent(ev events.Event) (up, known bool) {
	switch ev.Type {
	case events.MachineCreate, events.MachineStart, events.MachineReady, events.MachineSample:
		return true, true
	case events.MachineStop, events.MachineDestroy:
		return false, true
	case events.MachineState:
		st := provider.State(ev.State)
		return st == provider.Running || st == provider.Starting, true
	}
	return false, false
}

// loadUptime recovers the uptime of machines from the event log.
func (o *Orchestrator) loadUptime() {
	now := time.Now()
	evs, err := events.Read(o.stateDir, now.Add(-uptimeHistory), now)
	if err != nil {
		return
	}
	gone, seen := replayUptime(evs)
	o.removed = append(o.removed, gone...)
	o.trimRemovedLocked()
	o.seedUptime = seen
}

// maxRemoved bounds the destroyed machines kept for cost history (and
// listed by status).
const maxRemoved = 300

// trimRemovedLocked keeps the most recently removed machines.
func (o *Orchestrator) trimRemovedLocked() {
	if len(o.removed) <= maxRemoved {
		return
	}
	sort.Slice(o.removed, func(i, j int) bool { return o.removed[i].Removed.After(o.removed[j].Removed) })
	o.removed = append([]*Machine(nil), o.removed[:maxRemoved]...)
}

// mergeUptimeLocked gives a machine seen in the first listing after a
// restart the uptime recorded before it.
func (o *Orchestrator) mergeUptimeLocked(m *Machine, id string, st provider.State) {
	s, ok := o.seedUptime[id]
	if !ok {
		return
	}
	delete(o.seedUptime, id)
	m.Uptime, m.observed = s.Uptime, true
	if m.price <= 0 {
		m.price = s.price
	}
	// Up before the restart but not up now: it went down after its last
	// recorded event, not now (the caller observes the current state).
	if sp := &m.Uptime[len(m.Uptime)-1]; sp.End.IsZero() && st != provider.Running && st != provider.Starting {
		sp.End = s.listed
	}
}

// settleUptimeLocked ends the replay after the first listing: recorded
// machines it did not show are gone since their last event.
func (o *Orchestrator) settleUptimeLocked() {
	for _, m := range o.seedUptime {
		sp := &m.Uptime[len(m.Uptime)-1]
		if sp.End.IsZero() {
			sp.End = m.listed // gone after its last recorded event
		}
		m.Removed, m.State, m.listed = sp.End, provider.Failed, time.Time{}
		o.removed = append(o.removed, m)
	}
	o.seedUptime = nil
	o.trimRemovedLocked()
}
