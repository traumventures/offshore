package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/machine"
)

func TestStaticHostApplyAndPlatform(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	h := &StaticHost{SpecPath: filepath.Join(dir, "machine.json"), StateDir: filepath.Join(dir, "state"), UnitDir: dir, Binary: "/opt/cs",
		Docker: func(_ context.Context, _ []string, _ []byte, a ...string) ([]byte, error) {
			calls = append(calls, strings.Join(a, " "))
			return nil, nil
		},
		Systemctl: func(_ context.Context, a ...string) error {
			calls = append(calls, "systemctl "+strings.Join(a, " "))
			return nil
		}}
	ctx := context.Background()
	spec := Spec{Provider: StaticProvider, Layout: machine.Layout{ID: "m1", Image: "img", CacheVolume: "cs-cache-m1", DepsVolume: "cs-deps"}}
	if err := h.Apply(ctx, StaticPayload{Spec: spec, Secrets: map[string]string{"K": "v"}}); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(filepath.Join(dir, StaticUnit))
	if !strings.Contains(string(unit), "ExecStart=/opt/cs supervisor") {
		t.Fatalf("unit %s", unit)
	}
	p := &staticPlatform{stateDir: h.StateDir, host: h}
	m, err := p.FetchSecrets(ctx)
	if err != nil || m["K"] != "v" {
		t.Fatalf("secrets %v %v", m, err)
	}
	if err := p.DeleteSecrets(ctx); err != nil || p.DeleteSecrets(ctx) != nil {
		t.Fatal("delete secrets")
	}
	// A new machine replaces the old one: its containers and volumes go.
	calls = nil
	spec.Layout.ID = "m2"
	if err := h.Apply(ctx, StaticPayload{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, "\n")
	if !strings.Contains(got, "rm -f cs-m1") || !strings.Contains(got, "volume rm -f cs-cache-m1") {
		t.Fatalf("previous machine not removed: %s", got)
	}
	// Self-termination removes the machine, the unit is stopped last.
	calls = nil
	if err := p.TerminateSelf(ctx); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "systemctl disable --now "+StaticUnit {
		t.Fatalf("unit not stopped last: %v", calls)
	}
	if _, err := os.Stat(h.SpecPath); err == nil {
		t.Fatal("spec kept")
	}
}
