package scheduler

import (
	"strings"
	"testing"
)

// Production 2026-11-12: the disk of every machine was used beyond its
// budget by data outside the slots, so neither a prune nor a resize could
// cover a warm slot's growth; the task got no option at all and waited
// for capacity until its queue timeout (every task of the repository for
// an hour) while machines were started and idle-stopped around it. It
// must run, penalised.
func TestDiskTightStillRuns(t *testing.T) {
	m := machine("m", res(8, 30), Resources{})
	m.WarmRepos = []string{"r"}
	m.SlotBytes = map[string]int64{"r": 30 * gib}
	m.DiskTotal, m.DiskUsed, m.DiskBudget = 200*gib, 195*gib, 180*gib
	tk := task(0, 0)
	tk.Profile.SlotGrowth = 30 * gib
	pl := (Default{}).Place(Pool{Now: now, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || pl.Machine != "m" || !strings.Contains(pl.Reason, "disk tight") {
		t.Fatalf("task on a machine short of disk: %+v", pl)
	}
	// A prune that covers the growth stays the choice.
	m.SlotBytes["other"] = 100 * gib
	pl = (Default{}).Place(Pool{Now: now, Machines: []Machine{m}}, tk)
	if pl.Kind != Assign || strings.Contains(pl.Reason, "disk tight") || !strings.Contains(pl.Reason, "prune") {
		t.Fatalf("prunable disk: %+v", pl)
	}
}
