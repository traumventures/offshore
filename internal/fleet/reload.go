package fleet

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
)

// Reloader applies changes of the fleet configuration file to a Local
// fleet. Fleet settings change only by explicit reload: Check (the file
// changed since it was last read) or Reload (SIGHUP, cloudsourced reconcile).
// Requests never modify the fleet configuration.
type Reloader struct {
	path  string
	load  func(path string) (*config.FleetConfig, error)
	fleet *Local
	log   *slog.Logger

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

// NewReloader watches path, which the fleet's current configuration was just
// loaded from. load reads the file (config.LoadFleet plus adjustments).
func NewReloader(path string, load func(string) (*config.FleetConfig, error), f *Local, log *slog.Logger) *Reloader {
	return &Reloader{path: path, load: load, fleet: f, log: log, stamp: stampOf(path)}
}

// Path is the watched fleet configuration file.
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
		r.log.Warn("fleet configuration reload failed, keeping the previous one", "path", r.path, "err", err)
		return err
	}
	r.stamp = st
	for _, w := range u.Warnings {
		r.log.Warn("fleet configuration", "warning", w)
	}
	for _, w := range r.fleet.Reconfigure(u) {
		r.log.Warn("fleet configuration", "warning", w)
	}
	r.log.Info("fleet configuration applied", "path", r.path)
	return nil
}
