package worker

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/metrics"
)

func fakeMedium(magic int64, reflink bool, mountinfo string) hostfs.Probes {
	return hostfs.Probes{
		Statfs:    func(_ string, st *unix.Statfs_t) error { st.Type = magic; return nil },
		Mountinfo: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(mountinfo)), nil },
		Reflink:   func(string) bool { return reflink },
	}
}

const rootExt4 = "1 0 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n"

func TestCheckCacheMedium(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	// Cloud machine on ext4: degraded loudly.
	st := checkCacheMedium(t.TempDir(), hostfs.RequireXFS, fakeMedium(hostfs.MagicExt4, false, rootExt4), log)
	if st.OK || st.Type != "ext4" || len(st.Problems) != 2 || st.Policy != "xfs-reflink" {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatalf("no error logged:\n%s", logBuf.String())
	}
	// Cloud machine on xfs with reflink.
	logBuf.Reset()
	st = checkCacheMedium(t.TempDir(), hostfs.RequireXFS, fakeMedium(hostfs.MagicXFS, true, ""), log)
	if !st.OK || st.Type != "xfs" || !st.Reflink || len(st.Problems)+len(st.Warnings) != 0 {
		t.Fatalf("%+v", st)
	}
	if strings.Contains(logBuf.String(), "level=WARN") || strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatalf("clean medium logged a problem:\n%s", logBuf.String())
	}
	// local-docker on ext4 with noauto_da_alloc: warnings only.
	logBuf.Reset()
	st = checkCacheMedium(t.TempDir(), hostfs.Lenient, fakeMedium(hostfs.MagicExt4, false, "1 0 8:1 / / rw - ext4 /dev/sda1 rw,noauto_da_alloc\n"), log)
	if !st.OK || len(st.Problems) != 0 || len(st.Warnings) != 3 || !strings.Contains(strings.Join(st.Warnings, " "), "noauto_da_alloc") {
		t.Fatalf("%+v", st)
	}
	if strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "level=WARN") {
		t.Fatalf("lenient medium:\n%s", logBuf.String())
	}
}

func TestCacheMediumInStatusAndMetrics(t *testing.T) {
	s, err := New(Config{MachineID: "m1", CacheDir: t.TempDir(), CacheFSPolicy: hostfs.RequireXFS,
		FSProbes: fakeMedium(hostfs.MagicExt4, false, rootExt4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.CacheFS == nil || st.CacheFS.OK || st.CacheFS.Type != "ext4" {
		t.Fatalf("%+v", st.CacheFS)
	}
	var text bytes.Buffer
	metrics.WriteText(&text, st.Metrics)
	for _, want := range []string{`offshore_worker_cache_fs_ok{type="ext4",policy="xfs-reflink"} 0`, `offshore_worker_cache_fs_reflink 0`} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q in\n%s", want, text.String())
		}
	}
}

func TestCapturedIntact(t *testing.T) {
	files := t.TempDir()
	os.MkdirAll(filepath.Join(files, "target"), 0o755)
	os.WriteFile(filepath.Join(files, "target", "a.rlib"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(files, "b"), nil, 0o644)
	cs := &anyfs.ChangeSet{
		Nodes: []anyfs.Node{
			{Path: "target", Kind: anyfs.Dir},
			{Path: "target/a.rlib", Kind: anyfs.File, Size: 5},
			{Path: "b", Kind: anyfs.File, Size: 0},
		},
		Read: []string{"target/a.rlib", "b"},
	}
	if !capturedIntact(cs, files) {
		t.Fatal("complete capture rejected")
	}
	// A crash emptied the captured file before its data was on disk.
	os.WriteFile(filepath.Join(files, "target", "a.rlib"), nil, 0o644)
	if capturedIntact(cs, files) {
		t.Fatal("torn capture accepted")
	}
	os.Remove(filepath.Join(files, "b"))
	cs.Read = []string{"b"}
	if capturedIntact(cs, files) {
		t.Fatal("missing capture accepted")
	}
}
