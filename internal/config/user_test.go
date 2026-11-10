package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadUserFrom(t *testing.T, content string) (*User, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	return LoadUser()
}

func TestUserMultiProvider(t *testing.T) {
	u, err := loadUserFrom(t, `
[fleet]
cpu_overcommit    = 2.0
memory_overcommit = 1.25
soft_limit        = 1.2
swap              = true
limit_jobs        = true

[[fleet.size]]
provider = "local"
name     = "small"
cpu      = 4
memory   = "8G"
price_per_hour = 0.1

[[fleet.size]]
provider = "other"
name     = "big"
cpu      = 16
memory   = "32G"

[providers.local]
backend      = "local-docker"
command      = ["sudo", "-n", "docker"]
max_machines = 4

[providers.other]
backend = "local-docker"
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.ProviderNames(); len(got) != 2 || got[0] != "local" || got[1] != "other" {
		t.Fatalf("providers: %v", got)
	}
	if u.ProviderBackend("local") != "local-docker" || len(u.Fleet.Sizes) != 2 {
		t.Fatalf("unexpected: %+v", u.Fleet.Sizes)
	}
}

func TestUserLegacyMapsToLocal(t *testing.T) {
	u, err := loadUserFrom(t, "[fleet]\nmachine_cpu = 4\nmachine_memory = \"4G\"\n[docker]\ncommand = [\"docker\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if names := u.ProviderNames(); len(names) != 1 || names[0] != "local" {
		t.Fatalf("providers: %v", names)
	}
	if s := u.Fleet.Sizes; len(s) != 1 || s[0].Provider != "local" || s[0].CPU != 4 {
		t.Fatalf("sizes: %+v", s)
	}
}

func TestUserMemoryOvercommitNeedsSafety(t *testing.T) {
	if _, err := loadUserFrom(t, "[fleet]\nmemory_overcommit = 1.5\n"); err == nil {
		t.Fatal("memory overcommit without swap or soft_limit must be rejected")
	}
}

func TestUserSchedulingKeys(t *testing.T) {
	u, err := loadUserFrom(t, `
[fleet]
policy = "binpack"
max_machines = 4
consolidate_target = 0.6

[[fleet.warm_pool]]
days = ["mon", "Tuesday", "fri"]
from = "08:00"
to = "19:30"
machines = 2
timezone = "Europe/Berlin"
`)
	if err != nil {
		t.Fatal(err)
	}
	if u.Fleet.ConsolidateBelow == nil || *u.Fleet.ConsolidateBelow != 0.5 || u.Fleet.ConsolidateTarget != 0.6 {
		t.Fatalf("consolidation defaults: %+v", u.Fleet)
	}
	days, from, to, loc, err := u.Fleet.WarmPool[0].Parse()
	if err != nil || len(days) != 3 || days[1] != time.Tuesday || from != 8*time.Hour || to != 19*time.Hour+30*time.Minute || loc.String() != "Europe/Berlin" {
		t.Fatalf("warm pool: %v %v %v %v %v", days, from, to, loc, err)
	}
	off, err := loadUserFrom(t, "[fleet]\nconsolidate_below = 0\npolicy = \"classic\"\n")
	if err != nil || *off.Fleet.ConsolidateBelow != 0 {
		t.Fatalf("consolidate_below = 0 must turn consolidation off: %v", err)
	}
	for _, bad := range []string{
		"[fleet]\npolicy = \"fifo\"\n",
		"[fleet]\nconsolidate_below = 1.5\n",
		"[[fleet.warm_pool]]\ndays = [\"funday\"]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 1\n",
		"[[fleet.warm_pool]]\nfrom = \"8am\"\nto = \"10:00\"\nmachines = 1\n",
		"[[fleet.warm_pool]]\nfrom = \"08:00\"\nto = \"08:00\"\nmachines = 1\n",
		"[[fleet.warm_pool]]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 9\n",
		"[[fleet.warm_pool]]\nfrom = \"08:00\"\nto = \"10:00\"\nmachines = 1\ntimezone = \"Mars/Olympus\"\n",
	} {
		if _, err := loadUserFrom(t, bad); err == nil {
			t.Errorf("accepted invalid config:\n%s", bad)
		}
	}
}
