package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/provider"
)

// Prebuilder builds an environment image for every provider and platform
// of the pool ahead of the first session that needs it.
type Prebuilder interface {
	Prebuild(ctx context.Context, env Env, progress io.Writer) error
}

// EnvStater reports whether an environment image exists, without building.
type EnvStater interface {
	EnvState(ctx context.Context, hash, tag string) (EnvState, error)
}

// EnvState describes an environment image in the pool.
type EnvState struct {
	// Known: the pool has the build context (else the next run uploads it).
	Known bool `json:"known"`
	// Ready lists the image targets (provider/platform) holding the image,
	// Missing those where it will be built or pushed when needed.
	Ready   []string `json:"ready,omitempty"`
	Missing []string `json:"missing,omitempty"`
	// Building: a prebuild of the environment is running.
	Building bool   `json:"building,omitempty"`
	Error    string `json:"error,omitempty"` // last prebuild failure
}

var (
	_ Prebuilder = (*Local)(nil)
	_ EnvStater  = (*Local)(nil)
	_ Prebuilder = (*Remote)(nil)
	_ EnvStater  = (*Remote)(nil)
)

// imageTargets returns one (provider, size) per distinct image target.
func (l *Local) imageTargets() [][2]string {
	cfg := l.PoolConfig()
	var out [][2]string
	seen := map[string]bool{}
	for _, name := range sortedNames(l.provs) {
		if _, ok := l.provs[name].(provider.ImageHost); !ok {
			continue
		}
		sizes := []string{""}
		if cfg != nil {
			var ss []string
			for _, sz := range cfg.Pool.Sizes {
				if sz.Provider == name {
					ss = append(ss, sz.Name)
				}
			}
			if len(ss) > 0 {
				sizes = ss
			}
		}
		for _, size := range sizes {
			t, _ := l.target(name, size)
			key := name + "\x00" + string(t.Platform)
			if !seen[key] {
				seen[key] = true
				out = append(out, [2]string{name, size})
			}
		}
	}
	return out
}

// Prebuild registers env and builds (and pushes) its image for every
// provider and platform of the pool, so that the first machine of the
// environment does not wait for it. Concurrent builds of the same image
// (a run that needs it now) share the work through the image pipeline.
func (l *Local) Prebuild(ctx context.Context, env Env, progress io.Writer) error {
	l.Register(env)
	l.mu.Lock()
	if l.building == nil {
		l.building = map[string]int{}
		l.buildErr = map[string]string{}
	}
	l.building[env.Hash]++
	l.mu.Unlock()
	pw := &syncWriter{w: progress}
	targets := l.imageTargets()
	errs := make(chan error, len(targets))
	for _, t := range targets {
		go func(name, size string) {
			_, err := l.ensureImage(ctx, name, size, env, pw)
			errs <- err
		}(t[0], t[1])
	}
	var all []error
	for range targets {
		if err := <-errs; err != nil {
			all = append(all, err)
		}
	}
	err := errors.Join(all...)
	l.mu.Lock()
	l.building[env.Hash]--
	if l.building[env.Hash] <= 0 {
		delete(l.building, env.Hash)
	}
	if err != nil {
		l.buildErr[env.Hash] = err.Error()
	} else {
		delete(l.buildErr, env.Hash)
	}
	l.mu.Unlock()
	if err == nil {
		l.log.Info("environment prebuilt", "tag", env.Tag, "targets", len(targets))
	}
	return err
}

// EnvState checks each image target's registry for the environment image.
func (l *Local) EnvState(ctx context.Context, hash, tag string) (EnvState, error) {
	l.mu.Lock()
	_, known := l.envs[hash]
	st := EnvState{Known: known, Building: l.building[hash] > 0, Error: l.buildErr[hash]}
	l.mu.Unlock()
	for _, nt := range l.imageTargets() {
		t, _ := l.target(nt[0], nt[1])
		label := nt[0] + " (" + string(t.Platform) + ")"
		if t.Platform == "" {
			label = nt[0]
		}
		if _, ok := l.pipe.Ready(t, hash); ok {
			st.Ready = append(st.Ready, label)
			continue
		}
		ok := false
		if t.Registry != nil {
			if ref, err := t.Registry.Ref(ctx, tag, t.Platform); err == nil {
				ok, _ = t.Registry.Has(ctx, ref)
			}
		}
		if ok {
			st.Ready = append(st.Ready, label)
		} else {
			st.Missing = append(st.Missing, label)
		}
	}
	sort.Strings(st.Ready)
	sort.Strings(st.Missing)
	return st, nil
}

// Prebuild hands the environment to the control plane, which builds it in
// the background (detached from this request).
func (r *Remote) Prebuild(ctx context.Context, env Env, progress io.Writer) error {
	return r.ensureEnv(ctx, env, progress, url.Values{"prebuild": {"1"}})
}

// EnvState asks the control plane for the state of an environment image.
func (r *Remote) EnvState(ctx context.Context, hash, tag string) (EnvState, error) {
	var st EnvState
	q := pathEnv + "?" + url.Values{"hash": {hash}, "tag": {tag}, "state": {"1"}}.Encode()
	err := r.call(ctx, q, http.NoBody, "", nil, &st)
	return st, err
}

// Health checks /healthz (no credentials needed) and returns the control
// plane's clock from the Date header (zero when absent).
func (r *Remote) Health(ctx context.Context) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+pathHealth, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("healthz: %s", resp.Status)
	}
	t, _ := http.ParseTime(resp.Header.Get("Date"))
	return t, nil
}

// prebuildEnv runs a prebuild requested by a client, detached from the
// request: the build continues when the laptop disconnects.
func (s *Server) prebuildEnv(env Env) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if err := s.pool.Prebuild(ctx, env, io.Discard); err != nil {
			s.log.Warn("environment prebuild", "tag", env.Tag, "err", err)
		}
	}()
}
