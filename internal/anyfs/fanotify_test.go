package anyfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWatchAccessOverlay(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("fanotify needs root")
	}
	base := t.TempDir()
	if err := KernelOverlaySupported(base); err != nil {
		t.Skipf("no kernel overlay: %v", err)
	}
	lower := filepath.Join(base, "lower")
	must(t, os.MkdirAll(filepath.Join(lower, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(lower, "sub/r"), []byte("r"), 0o644))
	merged := filepath.Join(base, "merged")
	must(t, MountKernelOverlay(merged, filepath.Join(base, "upper"), filepath.Join(base, "work"), lower))
	defer Unmount(merged)
	var mu sync.Mutex
	got := map[string]bool{}
	w, err := WatchAccess(merged, func(p string, write bool) {
		mu.Lock()
		got[p] = write
		mu.Unlock()
	})
	if err != nil {
		t.Skipf("fanotify: %v", err)
	}
	defer w.Close()
	os.ReadFile(filepath.Join(merged, "sub/r")) // own reads are not reported
	must(t, exec.Command("cat", filepath.Join(merged, "sub/r")).Run())
	must(t, exec.Command("sh", "-c", "echo x > "+filepath.Join(merged, "w")).Run())
	bind := filepath.Join(base, "bind")
	os.MkdirAll(bind, 0o755)
	out, err := exec.Command("unshare", "-m", "sh", "-c", "mount --bind "+filepath.Join(merged, "sub")+" "+bind+" && cat "+bind+"/r").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("events: %v", got)
	if w, ok := got[filepath.Join(merged, "sub/r")]; !ok || w {
		t.Fatal("read not reported")
	}
	if w, ok := got[filepath.Join(merged, "w")]; !ok || !w {
		t.Fatal("write not reported")
	}
}
