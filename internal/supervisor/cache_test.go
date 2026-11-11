package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/clock"
	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/machine"
)

// fakeHost simulates mkfs/mount/blkid/findmnt/xfs_growfs.
type fakeHost struct {
	cmds    []string
	fs      map[string]bool   // device -> formatted (xfs)
	foreign map[string]string // device -> file system someone else made
	mounted string            // device mounted at the cache dir
}

func (h *fakeHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	h.cmds = append(h.cmds, name+" "+strings.Join(args, " "))
	dev := args[len(args)-1]
	switch name {
	case "findmnt":
		return []byte(h.mounted), nil
	case "blkid":
		if t := h.foreign[dev]; t != "" && !h.fs[dev] {
			return []byte(t + "\n"), nil
		}
		if h.fs[dev] {
			return []byte("xfs\n"), nil
		}
		return nil, nil
	case "mkfs.xfs":
		h.fs[dev] = true
	case "mount":
		h.mounted = args[len(args)-2]
	case "umount":
		h.mounted = ""
	}
	return nil, nil
}

func (h *fakeHost) count(prefix string) int {
	n := 0
	for _, c := range h.cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func cacheAgent(t *testing.T, c *CacheSpec) (*Supervisor, *fakeHost, *string) {
	a, _, _ := testAgent(t)
	h := &fakeHost{fs: map[string]bool{}}
	boot := "boot-1"
	a.Spec.Cache, a.Host, a.BootID, a.SysBlock = c, h.run, func() string { return boot }, t.TempDir()
	return a, h, &boot
}

func TestInstanceStoreFormattedEveryBoot(t *testing.T) {
	dir := t.TempDir()
	nvme := filepath.Join(dir, "nvme-Instance_Storage_1")
	os.WriteFile(nvme, nil, 0o600)
	a, h, boot := cacheAgent(t, &CacheSpec{Medium: CacheLocal, LocalDevices: filepath.Join(dir, "nvme-Instance_Storage_*")})
	ctx := context.Background()
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs -m reflink=1 -f "+nvme) != 1 || h.mounted != nvme || a.Spec.Layout.CacheVolume != a.cacheDir() {
		t.Fatalf("first boot: %v layout %q", h.cmds, a.Spec.Layout.CacheVolume)
	}
	// A supervisor restart in the same boot keeps the content.
	a.Spec.Layout.CacheVolume = "cs-cache"
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs") != 1 || a.Spec.Layout.CacheVolume != a.cacheDir() {
		t.Fatalf("restart reformatted: %v", h.cmds)
	}
	// After a stop/start the instance store is blank: format again.
	*boot = "boot-2"
	h.mounted = ""
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs") != 2 {
		t.Fatalf("new boot not formatted: %v", h.cmds)
	}
}

func TestLocalWithoutInstanceStoreUsesDockerVolume(t *testing.T) {
	a, h, _ := cacheAgent(t, &CacheSpec{Medium: CacheLocal, LocalDevices: filepath.Join(t.TempDir(), "none*")})
	if err := a.PrepareCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.cmds) != 0 || a.Spec.Layout.CacheVolume != "cs-cache" {
		t.Fatalf("%v %q", h.cmds, a.Spec.Layout.CacheVolume)
	}
}

func TestVolumeFormattedOnceAndExpired(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "vol0123")
	os.WriteFile(dev, nil, 0o600)
	a, h, _ := cacheAgent(t, &CacheSpec{Medium: CacheVolume, Device: dev, TTLSeconds: 3600})
	clk := clock.NewFake(time.Unix(1_000_000, 0))
	a.Clock = clk
	ctx := context.Background()
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs -m reflink=1 "+dev) != 1 || h.count("xfs_growfs") != 1 || a.Spec.Layout.CacheVolume != a.cacheDir() {
		t.Fatalf("new volume: %v", h.cmds)
	}
	// Content written, the volume detached and attached to a new machine
	// within its ttl: kept, not reformatted.
	os.WriteFile(filepath.Join(a.cacheDir(), "slot"), []byte("warm"), 0o600)
	h.mounted = ""
	clk.Advance(30 * time.Minute)
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs") != 1 {
		t.Fatalf("reformatted: %v", h.cmds)
	}
	if _, err := os.Stat(filepath.Join(a.cacheDir(), "slot")); err != nil {
		t.Fatal("warm content lost within the ttl")
	}
	var u cacheUse
	b, _ := os.ReadFile(filepath.Join(a.cacheDir(), cacheMarker))
	json.Unmarshal(b, &u)
	if u.Used != clk.Now().Unix() {
		t.Fatalf("marker %+v", u)
	}
	// Unused for longer than the ttl: emptied on attach.
	h.mounted = ""
	clk.Advance(2 * time.Hour)
	if err := a.PrepareCache(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.cacheDir(), "slot")); !os.IsNotExist(err) {
		t.Fatal("expired content kept")
	}
}

func TestVolumeRescanAndGrow(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "sdb")
	os.WriteFile(real, nil, 0o600)
	link := filepath.Join(dir, "oraclevdb")
	os.Symlink(real, link)
	a, h, _ := cacheAgent(t, &CacheSpec{Medium: CacheVolume, Device: link})
	rescan := filepath.Join(a.SysBlock, "sdb", "device", "rescan")
	os.MkdirAll(filepath.Dir(rescan), 0o755)
	os.WriteFile(rescan, nil, 0o600)
	if err := a.GrowCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rescan); string(b) != "1" || h.count("xfs_growfs "+a.cacheDir()) != 1 {
		t.Fatalf("rescan %q, cmds %v", b, h.cmds)
	}
}

func TestForeignVolumeReformattedAsXFS(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "vol0456")
	os.WriteFile(dev, nil, 0o600)
	a, h, _ := cacheAgent(t, &CacheSpec{Medium: CacheVolume, Device: dev})
	h.foreign = map[string]string{dev: "ext4"}
	if err := a.PrepareCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.count("mkfs.xfs -m reflink=1 -f "+dev) != 1 || h.mounted != dev {
		t.Fatalf("ext4 volume kept: %v", h.cmds)
	}
}

func TestMountedCacheChecked(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "vol0789")
	os.WriteFile(dev, nil, 0o600)
	a, _, _ := cacheAgent(t, &CacheSpec{Medium: CacheVolume, Device: dev})
	var logBuf bytes.Buffer
	a.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	a.FSProbes = hostfs.Probes{
		Statfs:    func(_ string, st *unix.Statfs_t) error { st.Type = hostfs.MagicXFS; return nil },
		Mountinfo: func() (io.ReadCloser, error) { return nil, errors.New("none") },
		Reflink:   func(string) bool { return false }, // xfs formatted without reflink
	}
	if err := a.PrepareCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "reflink") {
		t.Fatalf("no error for an xfs without reflink:\n%s", logBuf.String())
	}
}

func TestWorkerLayoutCacheFSPolicy(t *testing.T) {
	for prov, want := range map[string]string{"aws-ec2": "xfs-reflink", "oci": "xfs-reflink", StaticProvider: ""} {
		s := Spec{Provider: prov, Layout: machine.Layout{Env: map[string]string{"A": "1"}}}
		l := s.WorkerLayout()
		if l.Env[machine.CacheFSEnv] != want || l.Env["A"] != "1" {
			t.Errorf("%s: env %v", prov, l.Env)
		}
		if _, ok := s.Layout.Env[machine.CacheFSEnv]; ok {
			t.Errorf("%s: spec env mutated", prov)
		}
	}
}
