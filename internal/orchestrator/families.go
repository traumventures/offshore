package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

// Repository families: the worktrees of one repository (Request.Family,
// the client's user and git common directory) by repository id. A
// worktree without a head of its own is seeded from the newest head of a
// sibling (scheduler.Pool.Families); only build outputs are seeded,
// sources always come from the session's own worktree.

// familyKeep bounds the repositories remembered per family (the most
// recently seen).
const familyKeep = 32

func (o *Orchestrator) familiesPath() string { return filepath.Join(o.stateDir, "families.json") }

func (o *Orchestrator) loadFamilies() {
	o.families = map[string][]string{}
	if b, err := os.ReadFile(o.familiesPath()); err == nil {
		json.Unmarshal(b, &o.families)
	}
	if o.families == nil {
		o.families = map[string][]string{}
	}
}

// noteFamilyLocked records repo as a member of family (most recent last).
func (o *Orchestrator) noteFamilyLocked(family, repo string) {
	if family == "" || repo == "" {
		return
	}
	if o.families == nil {
		o.families = map[string][]string{}
	}
	rs := o.families[family]
	if n := len(rs); n > 0 && rs[n-1] == repo {
		return
	}
	rs = append(slices.DeleteFunc(rs, func(r string) bool { return r == repo }), repo)
	if len(rs) > familyKeep {
		rs = rs[len(rs)-familyKeep:]
	}
	o.families[family] = rs
	if o.stateDir == "" {
		return
	}
	if b, err := json.Marshal(o.families); err == nil {
		tmp := o.familiesPath() + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, o.familiesPath())
		}
	}
}
