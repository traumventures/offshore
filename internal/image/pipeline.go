package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Source is an environment to build. Context writes the build context to
// a temporary directory, which the pipeline removes.
type Source struct {
	Hash    string
	Tag     string
	Context func() (Spec, string, error)
}

// Target is where machines of one provider get their image from.
type Target struct {
	Name     string // provider name (messages, keys)
	Platform Platform
	Registry Registry
	Builder  Builder
}

// FailureBackoff is how long a failed preparation is remembered: requests
// in that window fail fast instead of rebuilding.
var FailureBackoff = 30 * time.Second

// Pipeline builds each environment once per builder and platform and
// pushes it once per registry reference. It is safe for concurrent use.
type Pipeline struct {
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	built  map[string]string // builder id + hash + platform -> local ref
	ready  map[string]string // target + hash + platform -> machine ref
	failed map[string]failure
}

type failure struct {
	err error
	at  time.Time
}

func (p *Pipeline) lock(key string) func() {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[string]*sync.Mutex{}
		p.built = map[string]string{}
		p.ready = map[string]string{}
		p.failed = map[string]failure{}
	}
	m, ok := p.locks[key]
	if !ok {
		m = &sync.Mutex{}
		p.locks[key] = m
	}
	p.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Ready returns the machine reference of an environment on a target if it
// was prepared before.
func (p *Pipeline) Ready(t Target, hash string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref, ok := p.ready[readyKey(t, hash)]
	return ref, ok
}

func readyKey(t Target, hash string) string {
	return t.Name + "\x00" + hash + "\x00" + string(t.Platform)
}

// Ensure makes src available to machines of t and returns the reference
// they run. The registry is asked first: when it has the image, nothing is
// built. log receives build and push output.
func (p *Pipeline) Ensure(ctx context.Context, src Source, t Target, log io.Writer) (string, error) {
	key := readyKey(t, src.Hash)
	unlock := p.lock("ready\x00" + key)
	defer unlock()
	p.mu.Lock()
	ref, ok := p.ready[key]
	f, failed := p.failed[key]
	p.mu.Unlock()
	if ok {
		return ref, nil
	}
	if failed && time.Since(f.at) < FailureBackoff {
		return "", f.err
	}
	ref, err := p.ensure(ctx, src, t, log)
	p.mu.Lock()
	if err != nil {
		if ctx.Err() == nil {
			p.failed[key] = failure{err, time.Now()}
		}
	} else {
		delete(p.failed, key)
		p.ready[key] = ref
	}
	p.mu.Unlock()
	return ref, err
}

func (p *Pipeline) ensure(ctx context.Context, src Source, t Target, log io.Writer) (string, error) {
	ref, err := t.Registry.Ref(ctx, src.Tag, t.Platform)
	if err != nil {
		return "", err
	}
	if ok, err := t.Registry.Has(ctx, ref); err != nil {
		return "", err
	} else if ok {
		return ref, nil
	}
	if t.Builder == nil {
		return "", fmt.Errorf("image %s is missing on %s and no builder is configured", ref, t.Name)
	}
	local, err := p.Build(ctx, src, t.Builder, t.Platform, log)
	if err != nil {
		return "", err
	}
	return t.Registry.Push(ctx, local, t.Platform, log)
}

// Build builds src for platform with b once and returns the local ref.
func (p *Pipeline) Build(ctx context.Context, src Source, b Builder, platform Platform, log io.Writer) (string, error) {
	key := b.ID() + "\x00" + src.Hash + "\x00" + string(platform)
	unlock := p.lock("build\x00" + key)
	defer unlock()
	p.mu.Lock()
	ref, ok := p.built[key]
	p.mu.Unlock()
	if ok {
		return ref, nil
	}
	spec, dir, err := src.Context()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ref, err = b.Build(ctx, spec, platform, log)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.built[key] = ref
	p.mu.Unlock()
	return ref, nil
}
