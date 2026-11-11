package pool

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/config"
)

// Reloader applies changes of the pool configuration file to a Local
// pool. Pool settings change only by explicit reload: Check (the file
// changed since it was last read) or Reload (SIGHUP, offshore reconcile).
// Requests never modify the pool configuration.
type Reloader struct {
	path string
	load func(path string) (*config.PoolConfig, error)
	pool *Local
	log  *slog.Logger

	mu    sync.Mutex
	stamp fileStamp
}

type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

// NewReloader watches path, which the pool's current configuration was just
// loaded from. load reads the file (config.LoadPool plus adjustments).
func NewReloader(path string, load func(string) (*config.PoolConfig, error), f *Local, log *slog.Logger) *Reloader {
	return &Reloader{path: path, load: load, pool: f, log: log, stamp: stampOf(path)}
}

// Path is the watched pool configuration file.
func (r *Reloader) Path() string { return r.path }

// Check reloads the configuration if the file changed since it was last
// read. It reports whether a new configuration was applied.
func (r *Reloader) Check() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stampOf(r.path) == r.stamp {
		return false
	}
	return r.reloadLocked() == nil
}

// Reload re-reads and applies the configuration unconditionally. On error
// the previous configuration stays in effect.
func (r *Reloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked()
}

func (r *Reloader) reloadLocked() error {
	st := stampOf(r.path)
	u, err := r.load(r.path)
	if err != nil {
		r.stamp = st // do not retry a broken file on every request
		r.log.Warn("pool configuration reload failed, keeping the previous one", "path", r.path, "err", err)
		return err
	}
	r.stamp = st
	for _, w := range u.Warnings {
		r.log.Warn("pool configuration", "warning", w)
	}
	for _, w := range r.pool.Reconfigure(u) {
		r.log.Warn("pool configuration", "warning", w)
	}
	r.log.Info("pool configuration applied", "path", r.path)
	return nil
}
