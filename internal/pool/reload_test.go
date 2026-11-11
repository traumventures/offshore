package pool

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
)

func TestReloaderAppliesOnlyChanges(t *testing.T) {
	defer func(b []string) { config.Backends = b }(config.Backends)
	config.Backends = append(config.Backends, "test-img")
	path := filepath.Join(t.TempDir(), "pool.toml")
	write := func(s string, mod time.Time) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, mod, mod)
	}
	base := "[pool]\nid = \"t\"\n[providers.img]\ntype = \"test-img\"\n\n[scheduler]\nestimator = \"static\"\n"
	t0 := time.Now().Add(-time.Hour)
	write(base, t0)
	u, err := config.LoadPool(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := NewLocal(u, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	load := func(p string) (*config.PoolConfig, error) { loads++; return config.LoadPool(p) }
	r := NewReloader(path, load, local, log)
	if r.Check() || loads != 0 {
		t.Fatal("unchanged file reloaded")
	}
	write("[pool]\nid = \"t\"\nmin_machines = 7\n[providers.img]\ntype = \"test-img\"\n\n[scheduler]\nestimator = \"static\"\n", t0.Add(time.Minute))
	if !r.Check() || local.PoolConfig().Pool.MinMachines != 7 {
		t.Fatalf("change not applied: %d", local.PoolConfig().Pool.MinMachines)
	}
	if r.Check() || loads != 1 {
		t.Fatalf("reloaded without change (%d loads)", loads)
	}
	// A broken file keeps the previous configuration and is not retried
	// until it changes again.
	write("[scheduler]\npolicy = \"fifo\"\n", t0.Add(2*time.Minute))
	if r.Check() || local.PoolConfig().Pool.MinMachines != 7 {
		t.Fatal("broken configuration applied")
	}
	if r.Check() || loads != 2 {
		t.Fatalf("broken file retried (%d loads)", loads)
	}
	if err := r.Reload(); err == nil {
		t.Fatal("explicit reload of a broken file must fail")
	}
	// Restart-only settings are reported, the pool id stays.
	write("[pool]\nid = \"other\"\n[providers.img]\ntype = \"test-img\"\nextra = 1\n\n[scheduler]\nestimator = \"static\"\n", t0.Add(3*time.Minute))
	nu, _ := config.LoadPool(path)
	ws := local.Reconfigure(nu)
	if len(ws) != 2 || local.orch.Config().Owner != "t" {
		t.Fatalf("warnings %v owner %q", ws, local.orch.Config().Owner)
	}
}
