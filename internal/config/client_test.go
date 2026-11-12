package config

import (
	"strings"
	"testing"
)

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// One file may serve the client and the embedded orchestrator: each loader
// reads its own sections.
func TestSingleFileEmbedded(t *testing.T) {
	p := writeFile(t, `
user = "alice"

[pool]
id = "team-a"
min_machines = 2

[providers.local]
type = "local-docker"
`)
	t.Setenv("OFFSHORE_CONFIG", p)
	t.Setenv("OFFSHORE_POOL_CONFIG", "")
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "alice" || !c.Embedded() || len(c.Warnings) != 0 {
		t.Fatalf("client %+v", c)
	}
	if got := strings.Join(c.PoolSections, ","); got != "[pool],[providers]" {
		t.Fatalf("pool sections %q", got)
	}
	if PoolConfigPath() != p {
		t.Fatalf("pool path %s", PoolConfigPath())
	}
	f, err := LoadPool(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Pool.ID != "team-a" || f.Pool.MinMachines != 2 || f.ProviderNames()[0] != "local" {
		t.Fatalf("pool %+v", f.Pool)
	}
	if !hasWarning(f.Warnings, "user ignored by the pool") {
		t.Fatalf("warnings %v", f.Warnings)
	}
}

func TestRemoteClientWarnsAboutPoolSections(t *testing.T) {
	p := writeFile(t, `
[control_plane]
url = "https://cs.example.com"

[pool]
min_machines = 9
`)
	t.Setenv("OFFSHORE_CONFIG", p)
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.ControlPlane.Mode != "remote" || c.ControlPlane.APIKey != "env:OFFSHORE_API_KEY" {
		t.Fatalf("orchestrator %+v", c.ControlPlane)
	}
	if !hasWarning(c.Warnings, "[pool] have no effect in remote mode") {
		t.Fatalf("warnings %v", c.Warnings)
	}
	// Without pool sections there is nothing to warn about.
	t.Setenv("OFFSHORE_CONFIG", writeFile(t, "[control_plane]\nurl = \"https://cs.example.com\"\n"))
	if c, err := LoadClient(); err != nil || len(c.Warnings) != 0 {
		t.Fatalf("%v %v", c.Warnings, err)
	}
}

func TestSeparatePoolFile(t *testing.T) {
	client := writeFile(t, "[pool]\nmin_machines = 9\n")
	t.Setenv("OFFSHORE_CONFIG", client)
	t.Setenv("OFFSHORE_POOL_CONFIG", writeFile(t, "[pool]\nid = \"f\"\n"))
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(c.Warnings, "have no effect, the pool configuration is") {
		t.Fatalf("warnings %v", c.Warnings)
	}
	f, err := LoadPool(PoolConfigPath())
	if err != nil || f.Pool.ID != "f" || f.Pool.MinMachines != 0 {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestPoolDefaults(t *testing.T) {
	t.Setenv("USER", "first last")
	f, err := LoadPool(writeFile(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if f.Pool.ID != "first-last" || len(f.Warnings) != 0 {
		t.Fatalf("id %q warnings %v", f.Pool.ID, f.Warnings)
	}
	// An absent file means defaults.
	if f, err := LoadPool(t.TempDir() + "/missing.toml"); err != nil || f.Pool.IdleStop != "" || f.Pool.MaxLifetime != "24h" {
		t.Fatalf("%v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"[pool]\nid = \"has space\"\n",
		"[pool]\nunknown_key = 1\n",
		"[nonsense]\nx = 1\n",
	} {
		if _, err := LoadPool(writeFile(t, bad)); err == nil {
			t.Errorf("pool accepted:\n%s", bad)
		}
	}
	for _, bad := range []string{
		"[control_plane]\n",
		"[control_plane]\nmode = \"magic\"\n",
		"[control_plane]\nurl = \"http://x\"\nfingerprint = \"ab\"\n",
		"[control_plane]\nurl = \"https://x\"\nclient_cert = \"c.pem\"\n",
		"user = \" \"\n",
		"[pool]\nunknown_key = 1\n",
	} {
		t.Setenv("OFFSHORE_CONFIG", writeFile(t, bad))
		if _, err := LoadClient(); err == nil {
			t.Errorf("client accepted:\n%s", bad)
		}
	}
	// Pool validation errors do not break the client (remote mode).
	t.Setenv("OFFSHORE_CONFIG", writeFile(t, "[control_plane]\nurl = \"https://x\"\n[scheduler]\npolicy = \"fifo\"\n"))
	if _, err := LoadClient(); err != nil {
		t.Fatal(err)
	}
}

// [outputs] cache_size bounds the daemon's output cache; the pool loader
// ignores it as a client setting, [writeback] is a pool section.
func TestClientOutputsCacheSize(t *testing.T) {
	p := writeFile(t, `
[outputs]
cache_size = "2G"

[writeback]
dirty_ratio = 0.2

[scheduler]
source_cache = "10%"
`)
	t.Setenv("OFFSHORE_CONFIG", p)
	t.Setenv("OFFSHORE_POOL_CONFIG", "")
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Outputs.CacheBytes(); got != 2<<30 {
		t.Fatalf("cache bytes %d", got)
	}
	if got := (ClientOutputs{}).CacheBytes(); got != 8<<30 {
		t.Fatalf("default cache bytes %d", got)
	}
	f, err := LoadPool(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Writeback.DirtyRatio != 0.2 || f.Pool.Scheduler.SourceCache != "10%" {
		t.Fatalf("pool %+v %+v", f.Writeback, f.Pool.Scheduler)
	}
	if !hasWarning(f.Warnings, "[outputs]") {
		t.Fatalf("warnings %v", f.Warnings)
	}
	bad := writeFile(t, "[outputs]\ncache_size = \"lots\"\n")
	if _, err := LoadClientFrom(bad); err == nil || !strings.Contains(err.Error(), "outputs.cache_size") {
		t.Fatalf("bad size: %v", err)
	}
}
