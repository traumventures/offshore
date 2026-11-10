package daemon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/manifest"
)

// root tracks one projected local directory: its manifest, versions and
// subscribers (machine sync streams).
type root struct {
	path    string
	scanner *manifest.Scanner
	log     *slog.Logger

	mu       sync.Mutex
	tree     manifest.Tree
	version  uint64
	subs     map[*rootSync]bool
	watcher  *fsnotify.Watcher
	watched  map[string]bool
	overflow bool
	lastUse  time.Time
	closed   bool
}

func rulesFor(r *config.Repo, rootPath string) manifest.Rules {
	rules := manifest.Rules{Gitignore: *r.Source.RespectGitignore}
	if r.Source.Default == config.ModeImmutable {
		rules.Default = manifest.ModeImmutable
	}
	if rootPath == r.Root {
		for _, rule := range r.Source.Rules {
			mr := manifest.Rule{Patterns: rule.Paths}
			switch rule.Mode {
			case config.ModeImmutable:
				mr.Mode = manifest.ModeImmutable
			case config.ModeHidden:
				mr.Mode = manifest.ModeHidden
			}
			rules.Rules = append(rules.Rules, mr)
		}
		for _, b := range r.Cache.Build.Paths {
			rules.Hidden = append(rules.Hidden, filepath.ToSlash(filepath.Clean(b)))
		}
	} else {
		for _, er := range r.Source.ExtraRoots {
			if er.Mode == config.ModeImmutable {
				rules.Default = manifest.ModeImmutable
			}
		}
	}
	return rules
}

func newRoot(path string, rules manifest.Rules, log *slog.Logger) (*root, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New(path + " is not a directory")
	}
	r := &root{path: path, scanner: manifest.NewScanner(path, rules), log: log, subs: map[*rootSync]bool{}, watched: map[string]bool{}}
	start := time.Now()
	tree, err := r.scanner.Scan(nil)
	if err != nil {
		return nil, err
	}
	r.tree = tree
	r.version = 1
	r.log.Info("scanned root", "root", path, "entries", len(tree), "duration", time.Since(start).Round(time.Millisecond))
	if w, err := fsnotify.NewWatcher(); err == nil {
		r.watcher = w
		r.addWatches()
		go r.watch()
	} else {
		r.log.Warn("file watcher unavailable, relying on scans before each run", "err", err)
	}
	return r, nil
}

func (r *root) setRules(rules manifest.Rules) {
	r.mu.Lock()
	r.scanner.Rules = rules
	r.mu.Unlock()
}

// addWatches registers watches for all visible watch-mode directories.
func (r *root) addWatches() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addWatchLocked("")
	for p, e := range r.tree {
		if e.Kind == manifest.Dir {
			r.addWatchLocked(p)
		}
	}
}

func (r *root) addWatchLocked(rel string) {
	if r.watcher == nil || r.watched[rel] {
		return
	}
	if rel != "" && r.scanner.ModeOf(rel, true) != manifest.ModeWatch {
		return
	}
	if err := r.watcher.Add(filepath.Join(r.path, filepath.FromSlash(rel))); err != nil {
		if !r.overflow {
			r.log.Warn("cannot watch directory, relying on scans before each run", "dir", rel, "err", err)
		}
		r.overflow = true
		return
	}
	r.watched[rel] = true
}

// watch turns file events into incremental rescans.
func (r *root) watch() {
	pending := map[string]bool{}
	full := false
	var timer <-chan time.Time
	for {
		select {
		case ev, ok := <-r.watcher.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(r.path, ev.Name)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = ""
			}
			if filepath.Base(ev.Name) == ".gitignore" || filepath.Base(ev.Name) == config.RepoFileName {
				full = true
			}
			pending[parentDir(rel)] = true
			if ev.Has(fsnotify.Create) {
				pending[rel] = true
			}
			if timer == nil {
				timer = time.After(30 * time.Millisecond)
			}
		case err, ok := <-r.watcher.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				r.log.Warn("file watcher overflow, rescanning", "root", r.path)
				full = true
				if timer == nil {
					timer = time.After(30 * time.Millisecond)
				}
			}
		case <-timer:
			timer = nil
			var err error
			if full {
				_, err = r.refresh()
			} else {
				dirs := make([]string, 0, len(pending))
				for d := range pending {
					dirs = append(dirs, d)
				}
				_, err = r.rescan(dirs)
			}
			if err != nil {
				r.log.Error("rescan", "root", r.path, "err", err)
			}
			pending = map[string]bool{}
			full = false
		}
	}
}

func parentDir(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return ""
	}
	return rel[:i]
}

// rescan re-reads directories and publishes the delta.
func (r *root) rescan(dirs []string) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, err := r.scanner.Rescan(r.tree, dirs)
	if err != nil {
		return r.version, err
	}
	r.publishLocked(d)
	return r.version, nil
}

// refresh performs a full scan (stat walk, hashing only changed files) and
// publishes the delta. It is the correctness barrier before every run.
func (r *root) refresh() (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := r.scanner.Scan(r.tree)
	if err != nil {
		return r.version, err
	}
	r.publishLocked(manifest.Diff(r.tree, next))
	return r.version, nil
}

func (r *root) publishLocked(d manifest.Delta) {
	if d.Empty() {
		return
	}
	r.tree.Apply(d)
	r.version++
	d.Version = r.version
	for _, e := range d.Upserts {
		if e.Kind == manifest.Dir {
			r.addWatchLocked(e.Path)
		}
	}
	for _, del := range d.Deletes {
		for w := range r.watched {
			if w == del || strings.HasPrefix(w, del+"/") {
				delete(r.watched, w)
			}
		}
	}
	for s := range r.subs {
		s.push(d)
	}
}

// subscribe attaches a sync stream and returns the full current state.
func (r *root) subscribe(s *rootSync) manifest.Delta {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[s] = true
	return manifest.Delta{Version: r.version, Full: true, Upserts: r.tree.Entries()}
}

func (r *root) unsubscribe(s *rootSync) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, s)
}

// entry returns the current manifest entry of a path.
func (r *root) entry(rel string) (manifest.Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tree[rel]
	return e, ok
}

func (r *root) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.watcher != nil {
		r.watcher.Close()
	}
}
