package pool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/image"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

// countingBuilder is a shared fake Docker builder.
type countingBuilder struct {
	mu     sync.Mutex
	builds []string
}

func (b *countingBuilder) ID() string { return "fake-docker" }
func (b *countingBuilder) Build(_ context.Context, s image.Spec, p image.Platform, log io.Writer) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.builds = append(b.builds, s.Tag+"@"+string(p))
	io.WriteString(log, "step 1/1\n")
	return image.LocalRef(s.Tag, p), nil
}

// pushRegistry is a fake remote registry recording pushes.
type pushRegistry struct {
	repo   string
	mu     sync.Mutex
	has    map[string]bool
	pushes []string
}

func (r *pushRegistry) Local() bool { return false }
func (r *pushRegistry) Ref(_ context.Context, tag string, p image.Platform) (string, error) {
	return image.RemoteTag(r.repo, tag, p), nil
}
func (r *pushRegistry) Has(_ context.Context, ref string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.has[ref], nil
}
func (r *pushRegistry) Push(_ context.Context, local string, p image.Platform, log io.Writer) (string, error) {
	ref := image.RemoteTag(r.repo, local, p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.has == nil {
		r.has = map[string]bool{}
	}
	r.has[ref] = true
	r.pushes = append(r.pushes, ref)
	io.WriteString(log, "pushed\n")
	return ref, nil
}

// hostProvider is a fake provider with an image target; Create records
// the image and fails (no machine is needed).
type hostProvider struct {
	name     string
	registry image.Registry
	builder  image.Builder
	mu       sync.Mutex
	created  []string
}

func (p *hostProvider) Name() string                        { return p.name }
func (p *hostProvider) Caps() provider.Caps                 { return provider.Caps{} }
func (p *hostProvider) ImagePlatform(string) image.Platform { return image.LinuxAMD64 }
func (p *hostProvider) ImageRegistry() image.Registry       { return p.registry }
func (p *hostProvider) ImageBuilder() image.Builder         { return p.builder }
func (p *hostProvider) Start(context.Context, string) (provider.Machine, error) {
	return provider.Machine{}, io.EOF
}
func (p *hostProvider) Create(_ context.Context, s provider.MachineSpec) (provider.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, s.Image)
	return provider.Machine{}, errors.New("no machines in this test")
}
func (p *hostProvider) Stop(context.Context, string) error    { return nil }
func (p *hostProvider) Destroy(context.Context, string) error { return nil }
func (p *hostProvider) List(context.Context, string) ([]provider.Machine, error) {
	return nil, nil
}

// localStore is the fake local Docker daemon.
type localStore struct{ b *countingBuilder }

func (localStore) Local() bool { return true }
func (localStore) Ref(_ context.Context, tag string, p image.Platform) (string, error) {
	return image.LocalRef(tag, p), nil
}
func (s localStore) Has(_ context.Context, ref string) (bool, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	for _, b := range s.b.builds {
		if strings.HasPrefix(b, ref+"@") {
			return true, nil
		}
	}
	return false, nil
}
func (localStore) Push(_ context.Context, ref string, _ image.Platform, _ io.Writer) (string, error) {
	return ref, nil
}

var testHosts sync.Map // backend name -> *hostProvider

func init() {
	provider.Register("test-host", func(opts map[string]any) (provider.Provider, error) {
		p, _ := testHosts.Load(opts["key"].(string))
		return p.(*hostProvider), nil
	})
}

func multiPool(t *testing.T, provs map[string]*hostProvider) *Local {
	u := &config.PoolConfig{Pool: config.Pool{ID: "t", MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s", MaxMachines: 4,
		Scheduler: config.Scheduler{Estimator: "static", CPUOvercommit: 1, MemoryOvercommit: 1}},
		Providers: map[string]map[string]any{}}
	for name, p := range provs {
		key := t.Name() + "/" + name
		testHosts.Store(key, p)
		u.Providers[name] = map[string]any{"type": "test-host", "key": key}
	}
	l, err := NewLocal(u, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestImagesArePushedOnlyWhereMachinesAreCreated(t *testing.T) {
	if image.Native() != image.LinuxAMD64 {
		t.Skip("the fake providers run linux/amd64")
	}
	b := &countingBuilder{}
	east, west := &pushRegistry{repo: "ecr/east"}, &pushRegistry{repo: "ecr/west"}
	provs := map[string]*hostProvider{
		"local": {name: "local-docker", registry: localStore{b}, builder: b},
		"east":  {name: "aws-ec2", registry: east, builder: b},
		"west":  {name: "aws-ec2", registry: west, builder: b},
	}
	l := multiPool(t, provs)
	env := testEnv(t)
	var progress bytes.Buffer
	if err := l.EnsureEnv(context.Background(), env, &progress); err != nil {
		t.Fatal(err)
	}
	if len(b.builds) != 1 || len(east.pushes)+len(west.pushes) != 0 {
		t.Fatalf("EnsureEnv: builds %v, pushes %v %v (want only the local build)", b.builds, east.pushes, west.pushes)
	}
	if !strings.Contains(progress.String(), "preparing environment image "+env.Tag+" on local") {
		t.Fatalf("progress %q", progress.String())
	}

	// A waiting acquire sees the push for the machine being created.
	waiter := &syncWriter{w: &bytes.Buffer{}}
	l.mu.Lock()
	l.waiters[env.Hash] = map[*syncWriter]bool{waiter: true}
	l.mu.Unlock()
	size := scheduler.Size{Provider: "east", Name: "c7a.xlarge", Capacity: scheduler.Resources{CPU: 4, Memory: 8 << 30}}
	if err := l.orch.CreateMachine(context.Background(), "m1", env.Hash, env.Tag, size); err == nil {
		t.Fatal("fake create should fail")
	}
	if got := provs["east"].created; len(got) != 1 || got[0] != "ecr/east:abcdef012345-amd64" {
		t.Fatalf("east machine image %v", got)
	}
	if len(east.pushes) != 1 || len(west.pushes) != 0 || len(b.builds) != 1 {
		t.Fatalf("after east create: builds %v, pushes %v %v", b.builds, east.pushes, west.pushes)
	}
	if out := waiter.w.(*bytes.Buffer).String(); !strings.Contains(out, "preparing environment image "+env.Tag+" on east") {
		t.Fatalf("waiter progress %q", out)
	}

	// A second machine on east reuses the push.
	l.orch.CreateMachine(context.Background(), "m2", env.Hash, env.Tag, size)
	if len(east.pushes) != 1 || len(provs["east"].created) != 2 {
		t.Fatalf("second east create pushed again: %v", east.pushes)
	}
	// The local provider runs the local tag.
	l.orch.CreateMachine(context.Background(), "m3", env.Hash, env.Tag, scheduler.Size{Provider: "local", Name: "default"})
	if got := provs["local"].created; len(got) != 1 || got[0] != env.Tag {
		t.Fatalf("local machine image %v", got)
	}
	if len(west.pushes) != 0 || len(b.builds) != 1 {
		t.Fatalf("west pushed without a machine: %v, builds %v", west.pushes, b.builds)
	}
}

func TestRemoteOnlyPoolDoesNotBuildWhenRegistryHasImage(t *testing.T) {
	b := &countingBuilder{}
	reg := &pushRegistry{repo: "ecr/r", has: map[string]bool{"ecr/r:abcdef012345-amd64": true}}
	provs := map[string]*hostProvider{"aws": {name: "aws-ec2", registry: reg, builder: b}}
	l := multiPool(t, provs)
	env := testEnv(t)
	env.Context = func() (provider.ImageSpec, string, error) {
		t.Error("the build context must not be needed")
		return provider.ImageSpec{}, "", errors.New("no context")
	}
	var progress bytes.Buffer
	if err := l.EnsureEnv(context.Background(), env, &progress); err != nil || progress.Len() != 0 {
		t.Fatalf("EnsureEnv %v %q", err, progress.String())
	}
	l.orch.CreateMachine(context.Background(), "m1", env.Hash, env.Tag, scheduler.Size{Provider: "aws", Name: "c7a.xlarge"})
	if len(b.builds) != 0 || len(reg.pushes) != 0 {
		t.Fatalf("builds %v pushes %v", b.builds, reg.pushes)
	}
	if got := provs["aws"].created; len(got) != 1 || got[0] != "ecr/r:abcdef012345-amd64" {
		t.Fatalf("image %v", got)
	}
}
