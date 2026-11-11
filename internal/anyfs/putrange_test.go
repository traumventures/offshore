package anyfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PutRange stores a page as a clone of a file range where the file system
// supports it, and always with the content that was read (b): a source
// written after reading is not cloned.
func TestLocalPutRange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenLocal(LocalOptions{Dir: filepath.Join(dir, "tier")})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	content := bytes.Repeat([]byte("0123456789abcdef"), 2<<16) // 2 MiB
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctime := fileCtime(f)
	page := content[1<<20:]
	k := KeyOf(page)
	if err := PutPage(ctx, l, k, page, Range{File: f, Off: 1 << 20, Ctime: ctime}, Dirty); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Get(ctx, k); err != nil || !bytes.Equal(got, page) {
		t.Fatalf("page content differs (err %v)", err)
	}
	t.Logf("cloned bytes %d (0 without reflink support)", l.Stats().ClonedBytes)

	// The source changes after it was read: the page still holds b.
	time.Sleep(10 * time.Millisecond)
	w, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteAt([]byte("CHANGED"), 0)
	w.Close()
	old := content[:1<<20]
	k2 := KeyOf(old)
	if err := l.PutRange(ctx, k2, old, Range{File: f, Off: 0, Ctime: ctime}, Dirty); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, k2); !bytes.Equal(got, old) {
		t.Fatal("page of a changed source holds the new content")
	}
}
