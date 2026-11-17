package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"
)

// A root holds the full manifest of its directory and an inotify watch on
// every directory in it. Agents create a worktree per task, so without
// eviction a long-running daemon accumulates one root per worktree it ever
// served (a 5-day daemon held 215 roots, about 0.9 GB live heap).
var (
	// rootIdleTTL is how long a root without attached machines stays
	// tracked after its last use; the next run rescans it from scratch.
	rootIdleTTL = 30 * time.Minute
	// rootSweepInterval is how often idle and vanished roots are evicted.
	rootSweepInterval = 5 * time.Minute
)

// sweepRoots evicts roots until ctx ends.
func (d *Daemon) sweepRoots(ctx context.Context) {
	t := time.NewTicker(rootSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.evictRoots(time.Now())
		}
	}
}

// evictRoots stops tracking roots whose directory no longer exists and
// roots unused since rootIdleTTL with no machine sync stream attached. A
// vanished root is detached from its machines too: nothing can be served
// from it any more. It returns the evicted paths.
func (d *Daemon) evictRoots(now time.Time) []string {
	d.mu.Lock()
	var evicted []*root
	var gone []string
	for p, r := range d.roots {
		_, err := os.Stat(p)
		vanished := errors.Is(err, fs.ErrNotExist)
		if !vanished && (now.Sub(r.lastUse) < rootIdleTTL || r.attached()) {
			continue
		}
		delete(d.roots, p)
		evicted = append(evicted, r)
		if vanished {
			gone = append(gone, p)
		}
	}
	conns := make([]*machineConn, 0, len(d.conns))
	for _, mc := range d.conns {
		conns = append(conns, mc)
	}
	d.mu.Unlock()

	for _, p := range gone {
		for _, mc := range conns {
			mc.detach(p)
		}
	}
	paths := make([]string, 0, len(evicted))
	for _, r := range evicted {
		r.close()
		paths = append(paths, r.path)
		d.log.Info("stopped tracking root", "root", r.path, "idle", now.Sub(r.lastUse).Round(time.Second))
	}
	return paths
}

// attached reports whether a machine sync stream is subscribed to the root.
func (r *root) attached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs) > 0
}

// detach stops the sync stream of a root on this machine, if any.
func (mc *machineConn) detach(path string) {
	mc.mu.Lock()
	rs, ok := mc.roots[path]
	if ok {
		delete(mc.roots, path)
	}
	mc.mu.Unlock()
	if ok {
		rs.stop(errors.New("root removed"))
	}
}
