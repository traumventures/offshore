package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/blobcache"
)

func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mt := time.Unix(1_700_000_000, 123456789)
	big := make([]byte, PackSize+12345)
	rand.Read(big)
	files := map[string][]byte{
		"target/debug/deps/libbig-0123456789abcdef.rlib": big,
		"target/debug/.fingerprint/a/lib":                []byte("fp"),
		"target/debug/empty":                             nil,
		"target/CACHEDIR.TAG":                            []byte("Signature: 8a477f597d28d172789f06886806bc55"),
	}
	for i := range 50 {
		files[filepath.Join("target/debug/build", string(rune('a'+i%26)), "out", string(rune('a'+i/26))+".o")] = bytes.Repeat([]byte{byte(i)}, 3000+i)
	}
	for p, b := range files {
		p = filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
		os.Chtimes(p, mt, mt)
	}
	os.Symlink("deps/libbig-0123456789abcdef.rlib", filepath.Join(dir, "target/debug/link"))
	os.Chmod(filepath.Join(dir, "target/debug/.fingerprint/a/lib"), 0o600)
	return dir
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	filepath.Walk(a, func(p string, fi os.FileInfo, err error) error {
		rel, _ := filepath.Rel(a, p)
		if rel == "." {
			return nil
		}
		q := filepath.Join(b, rel)
		gi, err := os.Lstat(q)
		if err != nil {
			t.Errorf("%s missing", rel)
			return nil
		}
		if fi.Mode() != gi.Mode() {
			t.Errorf("%s: mode %v, want %v", rel, gi.Mode(), fi.Mode())
		}
		if fi.Mode().IsRegular() {
			x, _ := os.ReadFile(p)
			y, _ := os.ReadFile(q)
			if !bytes.Equal(x, y) {
				t.Errorf("%s: content differs", rel)
			}
			if !fi.ModTime().Equal(gi.ModTime()) {
				t.Errorf("%s: mtime %v, want %v", rel, gi.ModTime(), fi.ModTime())
			}
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			x, _ := os.Readlink(p)
			y, _ := os.Readlink(q)
			if x != y {
				t.Errorf("%s: link %q, want %q", rel, y, x)
			}
		}
		return nil
	})
}

func TestSaveRestoreLazy(t *testing.T) {
	ctx := context.Background()
	src := tree(t)
	st, _ := blobcache.NewDirStore(t.TempDir(), time.Hour)
	m, stats, err := Save(ctx, src, st, Options{Meta: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewChunks < 3 || stats.Chunks != stats.NewChunks || stats.Files != 54 {
		t.Fatalf("stats %+v", stats)
	}
	// Small files are packed: far fewer chunks than files.
	if stats.Chunks > 10 {
		t.Fatalf("%d chunks for %d files", stats.Chunks, stats.Files)
	}
	got, err := Load(ctx, st, 0)
	if err != nil || got.At != m.At || string(got.Meta) != `{"x":1}` {
		t.Fatalf("load %v %v", got, err)
	}
	dst := filepath.Join(t.TempDir(), "slot")
	f := NewFiller(got, dst, st, filepath.Join(t.TempDir(), "chunks"))
	if err := f.Skeleton(); err != nil {
		t.Fatal(err)
	}
	rlib := "target/debug/deps/libbig-0123456789abcdef.rlib"
	fi, err := os.Stat(filepath.Join(dst, rlib))
	if err != nil || fi.Size() != PackSize+12345 || !f.Pending(rlib) {
		t.Fatalf("placeholder %v %v", fi, err)
	}
	if err := f.Fill(ctx, rlib); err != nil || f.Pending(rlib) {
		t.Fatalf("fill: %v", err)
	}
	f.Forget("target/debug/.fingerprint/a/lib") // replaced locally
	os.WriteFile(filepath.Join(dst, "target/debug/.fingerprint/a/lib"), []byte("fp"), 0o600)
	os.Chtimes(filepath.Join(dst, "target/debug/.fingerprint/a/lib"), time.Unix(1_700_000_000, 123456789), time.Unix(1_700_000_000, 123456789))
	if err := f.FillAll(ctx, 4); err != nil || f.Remaining() != 0 {
		t.Fatalf("fill all: %v (%d left)", err, f.Remaining())
	}
	sameTree(t, src, dst)
	f.Close()

	// A second save of an unchanged tree uploads nothing; a changed file
	// only its own chunk.
	m2, s2, err := Save(ctx, src, st, Options{Previous: m, Now: time.Now().Add(time.Second)})
	if err != nil || s2.NewChunks != 0 {
		t.Fatalf("unchanged save: %+v %v", s2, err)
	}
	p := filepath.Join(src, "target/debug/build/a/out/a.o")
	os.WriteFile(p, []byte("changed"), 0o644)
	_, s3, err := Save(ctx, src, st, Options{Previous: m2, Now: time.Now().Add(2 * time.Second)})
	if err != nil || s3.NewChunks != 1 {
		t.Fatalf("changed save: %+v %v", s3, err)
	}
}

func TestRestoreFailsOnCorruptChunk(t *testing.T) {
	ctx := context.Background()
	src := tree(t)
	dir := t.TempDir()
	st, _ := blobcache.NewDirStore(dir, time.Hour)
	m, _, err := Save(ctx, src, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := m.Entries[0].Refs
	for _, e := range m.Entries {
		if len(e.Refs) > 0 {
			h = e.Refs
			break
		}
	}
	st.Put(ctx, ChunkKey(h[0].Hash), bytes.NewReader([]byte("garbage")), 7)
	f := NewFiller(m, t.TempDir(), st, t.TempDir())
	f.Skeleton()
	if err := f.FillAll(ctx, 2); err == nil {
		t.Fatal("corrupt chunk accepted")
	}
}

func TestFuseFillLayer(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ctx := context.Background()
	src := tree(t)
	st, _ := blobcache.NewDirStore(t.TempDir(), time.Hour)
	m, _, _ := Save(ctx, src, st, Options{})
	backing := filepath.Join(t.TempDir(), "backing")
	f := NewFiller(m, backing, st, t.TempDir())
	if err := f.Skeleton(); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(t.TempDir(), "mnt")
	unmount, err := Mount(mp, f)
	if err != nil {
		t.Skipf("fuse mount not permitted: %v", err)
	}
	defer unmount()
	rlib := "target/debug/deps/libbig-0123456789abcdef.rlib"
	want, _ := os.ReadFile(filepath.Join(src, rlib))
	got, err := os.ReadFile(filepath.Join(mp, rlib))
	if err != nil || !bytes.Equal(got, want) || f.Pending(rlib) {
		t.Fatalf("read through the fill layer: %v", err)
	}
	if err := os.Remove(filepath.Join(mp, "target/CACHEDIR.TAG")); err != nil || f.Pending("target/CACHEDIR.TAG") {
		t.Fatalf("unlink: %v", err)
	}
}
