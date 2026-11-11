package daemon

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"github.com/traumventures/cloudsourced/internal/pool"
)

// prebuildDebounce delays the environment check after a change of an
// environment input, so an editor's save burst triggers one build.
var prebuildDebounce = 2 * time.Second

// envWatch starts environment image builds as soon as the environment of a
// repository changes (.cloudsourced.toml, the toolchain file, setup inputs,
// the Dockerfile directory), instead of the first run after the change
// waiting minutes for the image.
type envWatch struct {
	d *Daemon

	mu      sync.Mutex
	last    map[string]string      // root -> environment hash last seen
	inputs  map[string][]string    // root -> environment input paths (relative)
	timers  map[string]*time.Timer // root -> pending check
	builds  map[string]*Prebuild   // env hash -> latest prebuild
	running sync.WaitGroup
}

// Prebuild is the state of a background environment image build.
type Prebuild struct {
	Hash     string    `json:"hash"`
	Tag      string    `json:"tag"`
	Root     string    `json:"root"`
	Trigger  string    `json:"trigger"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Running  bool      `json:"running"`
	Error    string    `json:"error,omitempty"`
}

// EnvStatus is the result of op "env-status".
type EnvStatus struct {
	Hash     string         `json:"hash"`
	Tag      string         `json:"tag"`
	Prebuild *Prebuild      `json:"prebuild,omitempty"`
	Pool     *pool.EnvState `json:"pool,omitempty"`
	PoolErr  string         `json:"pool_error,omitempty"`
}

func newEnvWatch(d *Daemon) *envWatch {
	return &envWatch{d: d, last: map[string]string{}, inputs: map[string][]string{},
		timers: map[string]*time.Timer{}, builds: map[string]*Prebuild{}}
}

// envInputs lists the paths (relative to the root) whose change can change
// the environment hash.
func envInputs(r *config.Repo) []string {
	in := []string{config.RepoFileName, config.LocalFileName}
	if t := r.Environment.Toolchains.Rust; t != "" {
		in = append(in, filepath.ToSlash(filepath.Clean(t)))
	}
	if f := r.Environment.Dockerfile; f != "" {
		in = append(in, filepath.ToSlash(filepath.Dir(filepath.Clean(f))))
	}
	for _, s := range r.Environment.Setup {
		for _, p := range s.Inputs {
			in = append(in, filepath.ToSlash(filepath.Clean(p)))
		}
	}
	return in
}

// relevant reports whether a changed path is an environment input.
func relevant(inputs []string, path string) bool {
	for _, in := range inputs {
		if in == "." || path == in || strings.HasPrefix(path, in+"/") {
			return true
		}
	}
	return false
}

// observe records the environment of a repository at a request. A hash
// different from the last one seen is a change: the image build starts in
// the background right away (the request itself may need only part of it,
// e.g. the local image store, and other providers build ahead).
func (w *envWatch) observe(r *config.Repo, plan *envbuild.Plan, trigger string) {
	w.mu.Lock()
	w.inputs[r.Root] = envInputs(r)
	prev, seen := w.last[r.Root]
	w.last[r.Root] = plan.Hash
	w.mu.Unlock()
	if seen && prev != plan.Hash {
		w.start(r.Root, plan, trigger)
	}
}

// changed is called with the paths of a published manifest delta.
func (w *envWatch) changed(root string, paths []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	inputs, ok := w.inputs[root]
	if !ok {
		return
	}
	hit := ""
	for _, p := range paths {
		if relevant(inputs, p) {
			hit = p
			break
		}
	}
	if hit == "" {
		return
	}
	if t := w.timers[root]; t != nil {
		t.Stop()
	}
	w.timers[root] = time.AfterFunc(prebuildDebounce, func() { w.check(root, "changed "+hit) })
}

// check resolves the environment of root and starts a build if it changed.
func (w *envWatch) check(root, trigger string) {
	w.mu.Lock()
	delete(w.timers, root)
	w.mu.Unlock()
	repo, err := config.LoadRepo(root)
	if err != nil {
		w.d.log.Warn("environment prebuild: configuration", "root", root, "err", err)
		return
	}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		w.d.log.Warn("environment prebuild: resolve", "root", root, "err", err)
		return
	}
	w.observe(repo, plan, trigger)
}

// start builds the plan's image in the background unless a build of it is
// running or has succeeded.
func (w *envWatch) start(root string, plan *envbuild.Plan, trigger string) *Prebuild {
	w.mu.Lock()
	if b := w.builds[plan.Hash]; b != nil && (b.Running || b.Error == "") {
		w.mu.Unlock()
		return b
	}
	b := &Prebuild{Hash: plan.Hash, Tag: plan.Tag, Root: root, Trigger: trigger, Started: time.Now(), Running: true}
	w.builds[plan.Hash] = b
	w.running.Add(1)
	w.mu.Unlock()
	w.d.log.Info("environment changed, building image in the background", "root", root, "tag", plan.Tag, "trigger", trigger)
	go func() {
		defer w.running.Done()
		ctx := w.d.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		defer cancel()
		err := w.build(ctx, plan, io.Discard)
		w.finish(b, err)
	}()
	return b
}

func (w *envWatch) build(ctx context.Context, plan *envbuild.Plan, progress io.Writer) error {
	env := pool.Env{Hash: plan.Hash, Tag: plan.Tag, Context: plan.Context}
	if pb, ok := w.d.pool.(pool.Prebuilder); ok {
		return pb.Prebuild(ctx, env, progress)
	}
	return w.d.pool.EnsureEnv(ctx, env, progress)
}

func (w *envWatch) finish(b *Prebuild, err error) {
	w.mu.Lock()
	b.Running, b.Finished = false, time.Now()
	if err != nil {
		b.Error = err.Error()
	}
	w.mu.Unlock()
	if err != nil {
		w.d.log.Warn("environment prebuild failed", "tag", b.Tag, "err", err)
	} else {
		w.d.log.Info("environment prebuild done", "tag", b.Tag, "duration", time.Since(b.Started).Round(time.Second))
	}
}

// buildNow serves `cloudsourced env build`: start (or with wait, run and
// stream) the build of root's environment.
func (w *envWatch) buildNow(ctx context.Context, root string, wait bool, progress io.Writer) (*Prebuild, error) {
	repo, err := config.LoadRepo(root)
	if err != nil {
		return nil, err
	}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.inputs[repo.Root] = envInputs(repo)
	w.last[repo.Root] = plan.Hash
	w.mu.Unlock()
	if !wait {
		return w.start(repo.Root, plan, "cloudsourced env build"), nil
	}
	b := &Prebuild{Hash: plan.Hash, Tag: plan.Tag, Root: repo.Root, Trigger: "cloudsourced env build --wait", Started: time.Now(), Running: true}
	w.mu.Lock()
	w.builds[plan.Hash] = b
	w.mu.Unlock()
	fmt.Fprintf(progress, "cloudsourced: building environment image %s\n", plan.Tag)
	err = w.build(ctx, plan, progress)
	w.finish(b, err)
	return b, err
}

// status reports the environment of root: its hash, the daemon's latest
// prebuild and, when the pool can tell, where the image exists.
func (w *envWatch) status(ctx context.Context, root string) (*EnvStatus, error) {
	repo, err := config.LoadRepo(root)
	if err != nil {
		return nil, err
	}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		return nil, err
	}
	st := &EnvStatus{Hash: plan.Hash, Tag: plan.Tag}
	w.mu.Lock()
	if b := w.builds[plan.Hash]; b != nil {
		c := *b
		st.Prebuild = &c
	}
	w.mu.Unlock()
	if es, ok := w.d.pool.(pool.EnvStater); ok {
		ps, err := es.EnvState(ctx, plan.Hash, plan.Tag)
		if err != nil {
			st.PoolErr = err.Error()
		} else {
			st.Pool = &ps
		}
	}
	return st, nil
}
