package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadUserFrom(t *testing.T, content string) (*PoolConfig, error) {
	t.Helper()
	return LoadPool(writeFile(t, content))
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUserMultiProvider(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.local]
type      = "local-docker"
command      = ["sudo", "-n", "docker"]
max_machines = 4

[providers.other]
type = "local-docker"

[scheduler]
cpu_overcommit    = 2.0
memory_overcommit = 1.25
soft_limit        = 1.2
swap              = true
limit_jobs        = true

[[providers.local.size]]
name     = "small"
cpu      = 4
memory   = "8G"
price_per_hour = 0.1

[[providers.other.size]]
name     = "big"
cpu      = 16
memory   = "32G"
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.ProviderNames(); len(got) != 2 || got[0] != "local" || got[1] != "other" {
		t.Fatalf("providers: %v", got)
	}
	if u.ProviderType("local") != "local-docker" || len(u.Pool.Sizes) != 2 {
		t.Fatalf("unexpected: %+v", u.Pool.Sizes)
	}
}

func TestUserLegacyMapsToLocal(t *testing.T) {
	u, err := loadUserFrom(t, "[pool]\nmachine_cpu = 4\nmachine_memory = \"4G\"\n[providers.local]\ntype = \"local-docker\"\ncommand = [\"docker\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if names := u.ProviderNames(); len(names) != 1 || names[0] != "local" {
		t.Fatalf("providers: %v", names)
	}
	if s := u.Pool.Sizes; len(s) != 1 || s[0].Provider != "local" || s[0].CPU != 4 {
		t.Fatalf("sizes: %+v", s)
	}
}

func TestUserMemoryOvercommitNeedsSafety(t *testing.T) {
	if _, err := loadUserFrom(t, "[scheduler]\nmemory_overcommit = 1.5\n"); err == nil {
		t.Fatal("memory overcommit without swap or soft_limit must be rejected")
	}
}

func TestUserSchedulingKeys(t *testing.T) {
	u, err := loadUserFrom(t, `
[pool]
max_machines = 4

[scheduler]
policy = "binpack"
consolidate_target = 0.6

[[scheduler.warm_pool]]
days = ["mon", "Tuesday", "fri"]
from = "08:00"
to = "19:30"
machines = 2
timezone = "Europe/Berlin"
`)
	if err != nil {
		t.Fatal(err)
	}
	if u.Pool.ConsolidateBelow == nil || *u.Pool.ConsolidateBelow != 0.5 || u.Pool.ConsolidateTarget != 0.6 {
		t.Fatalf("consolidation defaults: %+v", u.Pool)
	}
	days, from, to, loc, err := u.Pool.WarmPool[0].Parse()
	if err != nil || len(days) != 3 || days[1] != time.Tuesday || from != 8*time.Hour || to != 19*time.Hour+30*time.Minute || loc.String() != "Europe/Berlin" {
		t.Fatalf("warm pool: %v %v %v %v %v", days, from, to, loc, err)
	}
	off, err := loadUserFrom(t, "[scheduler]\nconsolidate_below = 0\npolicy = \"classic\"\n")
	if err != nil || *off.Pool.ConsolidateBelow != 0 {
		t.Fatalf("consolidate_below = 0 must turn consolidation off: %v", err)
	}
	for _, bad := range []string{
		"[scheduler]\npolicy = \"fifo\"\n",
		"[scheduler]\nconsolidate_below = 1.5\n",
		"\n[scheduler]\n\n[[scheduler.warm_pool]]\ndays = [\"funday\"]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 1\n",
		"\n[scheduler]\n\n[[scheduler.warm_pool]]\nfrom = \"8am\"\nto = \"10:00\"\nmachines = 1\n",
		"\n[scheduler]\n\n[[scheduler.warm_pool]]\nfrom = \"08:00\"\nto = \"08:00\"\nmachines = 1\n",
		"\n[scheduler]\n\n[[scheduler.warm_pool]]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 9\n",
		"\n[scheduler]\n\n[[scheduler.warm_pool]]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 1\ntimezone = \"Mars/Olympus\"\n",
	} {
		if _, err := loadUserFrom(t, bad); err == nil {
			t.Errorf("accepted invalid config:\n%s", bad)
		}
	}
}

// The pool file schema: sizes nest under their provider, scheduling and
// machine setup have their own tables, and old key names are rejected.
func TestPoolSchema(t *testing.T) {
	f, err := LoadPool(writeFile(t, `
[pool]
id = "team"
max_machines = 3

[scheduler]
policy = "classic"
cpu_overcommit = 1.5

[[scheduler.warm_pool]]
from = "08:00"
to = "18:00"
machines = 1

[[machine.setup]]
run = "echo hi"

[providers.aws]
type = "aws-ec2"
region = "eu-central-1"

[[providers.aws.size]]
name = "small"
cpu = 4
memory = "8G"
`))
	if err != nil {
		t.Fatal(err)
	}
	p := f.Pool
	if p.ID != "team" || p.Policy != "classic" || p.CPUOvercommit != 1.5 || len(p.WarmPool) != 1 ||
		len(p.Provision) != 1 || len(p.Sizes) != 1 || p.Sizes[0].Provider != "aws" || f.ProviderType("aws") != "aws-ec2" {
		t.Fatalf("pool %+v", p)
	}
	if _, ok := f.Providers["aws"]["size"]; ok {
		t.Fatal("sizes left in the provider options")
	}
	for _, old := range []string{
		"[fleet]\nid = \"x\"\n",
		"[pool]\nowner = \"x\"\n",
		"[pool]\npolicy = \"classic\"\n",
		"[[pool.size]]\nname = \"s\"\ncpu = 1\nmemory = \"1G\"\n",
		"[[pool.provision]]\nrun = \"x\"\n",
		"[docker]\ncommand = [\"docker\"]\n",
		"[orchestrator]\nmode = \"embedded\"\n",
		"[providers.l]\ntype = \"local-docker\"\n[[providers.l.size]]\nprovider = \"l\"\nname = \"s\"\ncpu = 1\nmemory = \"1G\"\n",
	} {
		if _, err := LoadPool(writeFile(t, old)); err == nil {
			t.Errorf("accepted %q", old)
		}
	}
}

func TestCostModelKeys(t *testing.T) {
	u, err := loadUserFrom(t, "[scheduler]\ncost_optimization = 0.8\ndisk_price_per_gb_month = 0.1\n"+
		"[[scheduler.egress]]\nfrom = \"aws\"\nto = \"oci\"\nprice_per_gb = 0.09\nbandwidth = \"50M\"\n"+
		"[providers.aws]\ntype = \"aws-ec2\"\n[[providers.aws.size]]\nname = \"x\"\ncpu = 4\nmemory = \"8G\"\ndisk = \"200G\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if *u.Pool.CostOptimization != 0.8 || len(u.Pool.Egress) != 1 || u.Pool.Sizes[0].Disk != "200G" {
		t.Fatalf("got %+v", u.Pool.Scheduler)
	}
	if d, _ := loadUserFrom(t, ""); *d.Pool.CostOptimization != 0.5 {
		t.Fatal("default cost_optimization must be 0.5")
	}
	for _, bad := range []string{"[scheduler]\ncost_optimization = 1.5\n", "[scheduler]\nspeed = 0.5\n", "[[scheduler.egress]]\nbandwidth = \"fast\"\n"} {
		if _, err := loadUserFrom(t, bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
