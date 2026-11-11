package worker

import (
	"context"
	"path/filepath"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/access"
	"github.com/traumventures/offshore/internal/adapter"
	"github.com/traumventures/offshore/internal/anyfs"
)

// Access classes on the slot file system (internal/access): every commit
// feeds the repository's model (trees before and after; reads are not
// tracked yet), persisted next to its tier as access.json. The classes
// order reclaim (ephemeral, then churn materialized files go first) and
// pick what is prefetched when a slot is mounted fresh over a head
// (immutable and write-once files: the next build reads them).

const accessFile = "access.json"

// prefetchClassMax bounds the bytes a fresh mount prefetches by class.
const prefetchClassMax = 2 << 30

// loadAccess loads a repository's access model.
func loadAccess(repoDir string) *access.Model {
	return access.Load(filepath.Join(repoDir, accessFile), adapter.AccessPriors()...)
}

// observe folds a commit into the repository's access model.
func (c *cacheFS) observe(r *repoFS, before, after *anyfs.Tree, at time.Time) {
	r.access.Observe(access.Observation{Before: before, After: after, At: at})
	if err := r.access.Save(filepath.Join(c.dir, "repos", r.id, accessFile)); err != nil {
		c.log.Warn("save access classes", "repo", r.id, "err", err)
	}
}

// prefetchPlan returns the files of a tree to prefetch by class: larger
// than small (small files are prefetched anyway), policy Prefetch, in
// tree order up to budget bytes.
func prefetchPlan(m *access.Model, t *anyfs.Tree, small, budget int64) []*anyfs.Node {
	var out []*anyfs.Node
	var sum int64
	nodes := t.Nodes()
	for i := range nodes {
		n := &nodes[i]
		if !n.IsFile() || n.Size <= small || sum+n.Size > budget || !m.Policy(n.Path).Prefetch {
			continue
		}
		out = append(out, n)
		sum += n.Size
	}
	return out
}

// prefetchClasses starts the class prefetch of a freshly mounted slot;
// unmount cancels it.
func (c *cacheFS) prefetchClasses(sm *slotMount) {
	budget := int64(prefetchClassMax)
	if c.opt.Capacity > 0 {
		budget = min(budget, c.opt.Capacity/4)
	}
	nodes := prefetchPlan(sm.repo.access, sm.src.Tree(), prefetchSmall, budget)
	if len(nodes) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sm.cancel = cancel
	go func() {
		defer cancel()
		n := c.files.Prefetch(ctx, sm.repo.tier, nodes, 8)
		c.log.Debug("class prefetch", "repo", sm.repo.id, "files", n)
	}()
}

// coldOrder returns the files of the trees whose class is reclaimed
// before the rest, coldest last: churn (and unknown) first, ephemeral
// last.
func coldOrder(m *access.Model, trees []*anyfs.Tree) []*anyfs.Node {
	type cand struct {
		n     *anyfs.Node
		order int
	}
	var cs []cand
	for _, t := range trees {
		nodes := t.Nodes()
		for i := range nodes {
			n := &nodes[i]
			if !n.IsFile() {
				continue
			}
			if o := m.Policy(n.Path).ReclaimOrder; o < access.PolicyFor(access.Unknown).ReclaimOrder {
				cs = append(cs, cand{n, o})
			}
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].order > cs[j].order })
	out := make([]*anyfs.Node, len(cs))
	for i, x := range cs {
		out[i] = x.n
	}
	return out
}

// coldByClass marks the materialized files of the mounted slots' trees
// cold in class reclaim order, so that reclaim drops them first.
func (c *cacheFS) coldByClass() {
	c.mu.Lock()
	byRepo := map[*repoFS][]*anyfs.Tree{}
	for _, sm := range c.mounts {
		byRepo[sm.repo] = append(byRepo[sm.repo], sm.src.Tree())
	}
	c.mu.Unlock()
	for r, ts := range byRepo {
		c.files.Cold(coldOrder(r.access, ts))
	}
}
