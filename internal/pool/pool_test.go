package pool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/image"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
)

type imgProvider struct {
	mu     sync.Mutex
	builds []string
	files  map[string]string
}

func (p *imgProvider) Name() string                        { return "img" }
func (p *imgProvider) Caps() provider.Caps                 { return provider.Caps{} }
func (p *imgProvider) ImagePlatform(string) image.Platform { return image.LinuxAMD64 }
func (p *imgProvider) ImageRegistry() image.Registry       { return imgRegistry{p} }
func (p *imgProvider) ImageBuilder() image.Builder         { return p }
func (p *imgProvider) ID() string                          { return "img" }
func (p *imgProvider) Build(_ context.Context, s image.Spec, _ image.Platform, log io.Writer) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	df, err := os.ReadFile(s.Dockerfile)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(s.ContextDir, "sub", "data"))
	if err != nil {
		return "", err
	}
	link, _ := os.Readlink(filepath.Join(s.ContextDir, "link"))
	p.builds = append(p.builds, s.Tag)
	p.files = map[string]string{"Dockerfile": string(df), "data": string(data), "link": link}
	io.WriteString(log, "built\n")
	return s.Tag, nil
}

// imgRegistry is the provider's local image store.
type imgRegistry struct{ p *imgProvider }

func (imgRegistry) Local() bool { return true }
func (imgRegistry) Ref(_ context.Context, tag string, _ image.Platform) (string, error) {
	return tag, nil
}
func (r imgRegistry) Has(_ context.Context, ref string) (bool, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return slices.Contains(r.p.builds, ref), nil
}
func (imgRegistry) Push(_ context.Context, ref string, _ image.Platform, _ io.Writer) (string, error) {
	return ref, nil
}
func (p *imgProvider) Create(context.Context, provider.MachineSpec) (provider.Machine, error) {
	return provider.Machine{}, io.EOF
}
func (p *imgProvider) Start(context.Context, string) (provider.Machine, error) {
	return provider.Machine{}, io.EOF
}
func (p *imgProvider) Stop(context.Context, string) error    { return nil }
func (p *imgProvider) Destroy(context.Context, string) error { return nil }
func (p *imgProvider) List(context.Context, string) ([]provider.Machine, error) {
	return nil, nil
}

func init() {
	provider.Register("test-img", func(map[string]any) (provider.Provider, error) { return theProvider, nil })
}

var theProvider = &imgProvider{}

func testEnv(t *testing.T) Env {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "data"), []byte("payload"), 0o644)
	os.Symlink("sub/data", filepath.Join(src, "link"))
	return Env{Hash: "abcdef0123456789", Tag: "offshore-env:abcdef012345", Context: func() (provider.ImageSpec, string, error) {
		return provider.ImageSpec{Tag: "offshore-env:abcdef012345", ContextDir: src, Dockerfile: filepath.Join(src, "Dockerfile")}, t.TempDir(), nil
	}}
}

func TestControlPlaneRoundTrip(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{ID: "t", MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s",
		Scheduler: config.Scheduler{Estimator: "static", CPUOvercommit: 1, MemoryOvercommit: 1}},
		Providers: map[string]map[string]any{"img": {"type": "test-img"}}}
	state := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := NewLocal(u, state, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(local, "secret-key-0123456789", state, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv)
	defer ts.Close()
	sum := sha256.Sum256(ts.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])
	ctx := context.Background()

	bad, _ := NewRemote(ts.URL, fp, "wrong-key-0123456789")
	if _, err := bad.Status(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong key must be rejected, got %v", err)
	}
	pinned, _ := NewRemote(ts.URL, strings.Repeat("0", 64), "secret-key-0123456789")
	if _, err := pinned.Status(ctx); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("fingerprint mismatch must be rejected, got %v", err)
	}

	r, _ := NewRemote(ts.URL, fp, "secret-key-0123456789")
	env := testEnv(t)
	var progress bytes.Buffer
	if err := r.EnsureEnv(ctx, env, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "uploading environment") {
		t.Fatalf("first EnsureEnv should upload: %q", progress.String())
	}
	if got := theProvider.files; got["Dockerfile"] != "FROM scratch\n" || got["data"] != "payload" || got["link"] != "sub/data" {
		t.Fatalf("context not transferred: %v", got)
	}
	progress.Reset()
	if err := r.EnsureEnv(ctx, env, &progress); err != nil || strings.Contains(progress.String(), "uploading") {
		t.Fatalf("second EnsureEnv must not upload: %v %q", err, progress.String())
	}
	if n := len(theProvider.builds); n != 1 {
		t.Fatalf("image built %d times", n)
	}

	// A restarted control plane knows the stored context without upload.
	local2, _ := NewLocal(u, state, log)
	if _, err := NewServer(local2, "secret-key-0123456789", state, log); err != nil {
		t.Fatal(err)
	}
	if !local2.Known(env.Hash) {
		t.Fatal("stored environment not registered after restart")
	}

	fallback := scheduler.Resources{CPU: 2, Memory: 1 << 30}
	got, err := r.Estimate(ctx, "k", fallback)
	if err != nil || got.Need != fallback || got.MemorySource != scheduler.SourceFallback {
		t.Fatalf("estimate: %v %v", got, err)
	}
	if err := r.Release(ctx, "lease", "k", &scheduler.Usage{PeakMemory: 1}); err != nil {
		t.Fatal(err)
	}
	if ms, err := r.Status(ctx); err != nil || len(ms) != 0 {
		t.Fatalf("status: %v %v", ms, err)
	}
	if q, err := r.Queue(ctx); err != nil || len(q) != 0 {
		t.Fatalf("queue: %v %v", q, err)
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("x"), 0o644)
	if err := PackEnv(&buf, provider.ImageSpec{Tag: "t", ContextDir: dir, Dockerfile: filepath.Join(dir, "Dockerfile")}); err != nil {
		t.Fatal(err)
	}
	if _, err := UnpackEnv(bytes.NewReader(buf.Bytes()), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := PackEnv(&buf, provider.ImageSpec{Tag: "t", ContextDir: dir, Dockerfile: "/etc/passwd"}); err == nil {
		t.Fatal("dockerfile outside the context must be rejected")
	}
}

func TestConfigMapsSchedulingKeys(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s",
		Scheduler: config.Scheduler{
			WarmPool: []config.WarmPool{{Days: []string{"mon"}, From: "08:00", To: "18:00", Machines: 1, Timezone: "UTC"}}}}}
	p := Config(u).Policy
	if p.IdleStop != time.Minute || len(p.WarmPool) != 1 ||
		p.WarmPool[0].From != 8*time.Hour || p.WarmPool[0].To != 18*time.Hour || p.WarmPool[0].Days[0] != time.Monday || p.WarmPool[0].Machines != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestConfigDiskBudget(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s",
		Scheduler: config.Scheduler{DiskBudget: "85%"},
		Sizes:     []config.Size{{Provider: "p", Name: "a", CPU: 2, Memory: "4G"}, {Provider: "p", Name: "b", CPU: 2, Memory: "4G", DiskBudget: "150G"}}}}
	s := Config(u).Sizes
	if s[0].DiskBudget != "85%" || s[1].DiskBudget != "150G" {
		t.Fatalf("budgets %q %q", s[0].DiskBudget, s[1].DiskBudget)
	}
}

func TestConfigTierStack(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s",
		Sizes: []config.Size{{Provider: "p", Name: "l", CPU: 2, Memory: "4G", Disk: "50G"},
			{Provider: "p", Name: "lvs", CPU: 2, Memory: "4G", Stack: []string{"local", "volume", "store"}}}}}
	s := Config(u).Sizes
	if got := s[0].Stack; got.String() != "local>store" || got.Capacity != 50<<30 || s[0].Medium != "local" {
		t.Fatalf("local size: %+v", got)
	}
	if got := s[1].Stack; got.String() != "local>volume>store" || got.Name != "local>volume>store" || s[1].Medium != "volume" || s[1].Volume.Min == 0 {
		t.Fatalf("stacked size: %+v %+v", got, s[1])
	}
}

func TestProviderEnabledInPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := "[pool]\nmax_spend_per_hour = 2.5\n[providers.a]\ntype = \"local-docker\"\nenabled = false\n[providers.b]\ntype = \"local-docker\"\nenabled = true\n" +
		"[[providers.a.size]]\nname = \"s\"\ncpu = 2\nmemory = \"4G\"\n[[providers.b.size]]\nname = \"s\"\ncpu = 2\nmemory = \"4G\"\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := config.LoadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	p := Config(u).Policy
	if !p.Disabled("a") || p.Disabled("b") || p.MaxSpendPerHour != 2.5 {
		t.Fatalf("disabled %v, max spend %v", p.DisabledProviders, p.MaxSpendPerHour)
	}
}
