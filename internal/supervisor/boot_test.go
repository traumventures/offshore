package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/machine"
)

func TestBootPhases(t *testing.T) {
	a, _, _ := testAgent(t)
	script := filepath.Join(t.TempDir(), "boot-phases")
	now := time.Now().UnixMilli()
	if err := os.WriteFile(script, []byte("user_data "+itoa(now-3000)+"\nbinary "+itoa(now-2000)+"\ngarbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.BootScriptPath = script
	a.Uptime = func() time.Duration { return 9 * time.Second }
	a.startBoot()
	if err := a.EnsureContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	ph, err := machine.ReadBoot(filepath.Join(a.Spec.Layout.StatusDir, "boot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ph {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "kernel,user_data,binary,supervisor,registry_login,image,secrets,containers" {
		t.Fatalf("phases %s", got)
	}
	if k := ph[0].At; k > now-8000 || k < now-11000 {
		t.Fatalf("kernel start %d, want about %d", k, now-9000)
	}
	// A later boot (a started machine) has no boot script phases.
	a.BootScriptPath = filepath.Join(t.TempDir(), "none")
	a.startBoot()
	if len(a.boot) != 2 || a.boot[1].Name != machine.PhaseSupervisor {
		t.Fatalf("restart phases %+v", a.boot)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

type updatingPlatform struct {
	*fakePlatform
	updated bool
	err     error
	path    string
}

func (p *updatingPlatform) UpdateBinary(_ context.Context, path string) (bool, error) {
	p.path = path
	return p.updated, p.err
}

func TestUpdateBinary(t *testing.T) {
	a, fp, _ := testAgent(t)
	up := &updatingPlatform{fakePlatform: fp}
	a.Platform = up
	var execd []string
	a.Exec = func(p string) error { execd = append(execd, p); return nil }
	if a.updateBinary(context.Background()) || len(execd) != 0 || up.path != "/bin/cs" {
		t.Fatalf("nothing pending: exec %v path %q", execd, up.path)
	}
	up.updated = true
	if !a.updateBinary(context.Background()) || len(execd) != 1 || execd[0] != "/bin/cs" {
		t.Fatalf("pending binary not executed: %v", execd)
	}
	// A failed exec keeps the running supervisor.
	a.Exec = func(string) error { return errors.New("exec format error") }
	if a.updateBinary(context.Background()) {
		t.Fatal("failed exec must continue with the old process")
	}
}

func TestReplaceBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offshore")
	os.WriteFile(path, []byte("old"), 0o755)
	if err := ReplaceBinary(path, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if string(b) != "new" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("%q %v", b, fi.Mode())
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary file left")
	}
}
