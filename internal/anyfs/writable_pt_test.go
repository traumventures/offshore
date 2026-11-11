package anyfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFilesLimitReclaimsClosedLRU(t *testing.T) {
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{}
	a, b, c := p.file("a", strings.Repeat("a", 400)), p.file("b", strings.Repeat("b", 400)), p.file("c", strings.Repeat("c", 400))
	for _, n := range []*Node{&a, &b} {
		if _, err := files.EnsureWith(ctx, n, p.fetch); err != nil {
			t.Fatal(err)
		}
	}
	fa, ok := files.Open(&a) // a is the oldest but open
	if !ok {
		t.Fatal("a not resident")
	}
	files.SetLimit(1000)
	if _, err := files.EnsureWith(ctx, &c, p.fetch); err != nil { // 1200 > 1000: reclaim to 900
		t.Fatal(err)
	}
	if _, ok := files.Local(&a); !ok {
		t.Fatal("open file reclaimed")
	}
	if _, ok := files.Local(&b); ok {
		t.Fatal("closed LRU file not reclaimed")
	}
	if _, ok := files.Local(&c); !ok {
		t.Fatal("just faulted file reclaimed")
	}
	if b, _ := io.ReadAll(fa); len(b) != 400 {
		t.Fatalf("open file content %d bytes", len(b))
	}
	fa.Close()
	files.Release(&a)
	if _, ok := files.Open(&b); ok {
		t.Fatal("Open of a reclaimed file succeeded")
	}
	files.SetLimit(500)
	if st := files.Stats(); st.ResidentBytes > 450 {
		t.Fatalf("resident %d after SetLimit(500)", st.ResidentBytes)
	}

	// Files found at start are unbacked until MarkAllBacked.
	again, err := OpenFiles(files.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Reclaim(0) != 0 {
		t.Fatal("unbacked file reclaimed")
	}
	again.MarkAllBacked()
	if again.Reclaim(0) == 0 {
		t.Fatal("MarkAllBacked: nothing reclaimed")
	}
}

func mountPeerWritable(t *testing.T, p *peer, opt WritableOptions, nodes ...Node) (*WritableFS, string) {
	t.Helper()
	files, err := OpenFiles(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWritable(NewPeerSource(NewTree(nodes), files, p.fetch), t.TempDir(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return w, mountWritableOrSkip(t, w)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

// TestWritablePassthrough needs root (passthrough registers backing files
// with CAP_SYS_ADMIN): go test -c && sudo -n ./anyfs.test -test.run Passthrough
func TestWritablePassthrough(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("passthrough needs root")
	}
	content := strings.Repeat("0123456789abcdef", 64<<10) // 1 MiB
	reads := func(noPT bool) (WritableStats, bool) {
		p := &peer{}
		w, mnt := mountPeerWritable(t, p, WritableOptions{NoPassthrough: noPT}, dir("src"), p.file("src/big", content))
		for range 5 {
			b, err := os.ReadFile(filepath.Join(mnt, "src", "big"))
			if err != nil || string(b) != content {
				t.Fatalf("read: %d bytes, %v", len(b), err)
			}
		}
		return w.Stats(), w.Passthrough()
	}
	without, _ := reads(true)
	with, pt := reads(false)
	t.Logf("5 reads of 1 MiB: daemon reads without passthrough %d, with %d (opens %d, passthrough %d)",
		without.Reads, with.Reads, with.Opens, with.PassthroughOpens)
	if !pt {
		t.Skip("kernel did not accept passthrough")
	}
	if with.Reads != 0 || with.PassthroughOpens != 5 || without.Reads == 0 {
		t.Fatalf("with %+v, without %+v", with, without)
	}

	// Transitions: a write of a file open for passthrough reading waits
	// for the readers (the kernel ties the inode to the clean backing
	// file) and fails with ETXTBSY if they stay; once they are gone the
	// write copies the file up, and the path stays out of passthrough.
	// A change on the laptop gives the path a new inode while an old
	// handle is open.
	defer func(d time.Duration) { passthroughWait = d }(passthroughWait)
	passthroughWait = 100 * time.Millisecond
	p := &peer{}
	w, mnt := mountPeerWritable(t, p, WritableOptions{}, dir("src"), p.file("src/a", "v1\n"), p.file("src/b", "v1\n"))
	at := filepath.Join(mnt, "src", "a")
	r1, err := os.Open(at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.OpenFile(at, os.O_WRONLY|os.O_APPEND, 0); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("write open while a passthrough handle is open: %v", err)
	}
	if b, _ := io.ReadAll(r1); string(b) != "v1\n" {
		t.Fatalf("clean handle: %q", b)
	}
	r1.Close()
	var f *os.File
	eventually(t, "write open after the reader closed", func() bool {
		f, err = os.OpenFile(at, os.O_WRONLY|os.O_APPEND, 0)
		return err == nil
	})
	if _, err := f.WriteString("local\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if b, err := os.ReadFile(at); err != nil || string(b) != "v1\nlocal\n" {
		t.Fatalf("after copy-up: %q %v", b, err)
	}
	if st := w.Stats(); st.CopyUps != 1 || st.Writes == 0 {
		t.Fatalf("stats after write: %+v", st)
	}
	w.Apply([]Node{p.file("src/a", "v1\nlocal\n")}, false, 1) // written back
	ptn := w.Stats().PassthroughOpens
	if b, err := os.ReadFile(at); err != nil || string(b) != "v1\nlocal\n" {
		t.Fatalf("after write back: %q %v", b, err)
	}
	if w.Stats().PassthroughOpens != ptn {
		t.Fatal("written path opened for passthrough")
	}

	at = filepath.Join(mnt, "src", "b")
	w.Apply([]Node{p.file("src/b", "v2 from laptop\n")}, false, 2)
	r2, err := os.Open(at)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	w.Apply([]Node{p.file("src/b", "v3\n")}, false, 3)
	if b, err := os.ReadFile(at); err != nil || string(b) != "v3\n" {
		t.Fatalf("after laptop change: %q %v", b, err)
	}
	if b, _ := io.ReadAll(r2); string(b) != "v2 from laptop\n" {
		t.Fatalf("old handle after laptop change: %q", b)
	}
	if fi, err := os.Stat(at); err != nil || fi.Size() != 3 {
		t.Fatalf("stat after change: %v %v", fi, err)
	}

	// Open files are never reclaimed, closed ones are (releases reach
	// the daemon asynchronously).
	files := w.src.Files()
	v2 := p.file("src/b", "v2 from laptop\n")
	eventually(t, "closed files not reclaimed", func() bool {
		files.Reclaim(0)
		return len(files.Digests()) == 1
	})
	if _, ok := files.Local(&v2); !ok {
		t.Fatal("open file reclaimed")
	}
	r2.Close()
	eventually(t, "file not reclaimed after close", func() bool {
		files.Reclaim(0)
		return len(files.Digests()) == 0
	})
	if b, err := os.ReadFile(at); err != nil || string(b) != "v3\n" {
		t.Fatalf("refault after reclaim: %q %v", b, err)
	}
}
