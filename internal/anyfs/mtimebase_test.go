package anyfs

import (
	"bytes"
	"testing"
)

func TestMtimeBasePresentation(t *testing.T) {
	w, err := NewWritable(NewPeerSource(nil, nil, nil), t.TempDir(), WritableOptions{})
	if err != nil {
		t.Fatal(err)
	}
	file := func(p, d string, mt int64) Node {
		return Node{Path: p, Kind: File, Mode: 0o644, Digest: d, MtimeNs: mt}
	}
	w.Apply([]Node{file("same", "aaaa", 900), file("other", "bbbb", 50), file("new", "cccc", 950)}, true, 1)
	mtime := func(p string) int64 {
		v, ok := w.stat(p)
		if !ok {
			t.Fatalf("%s missing", p)
		}
		return v.MtimeNs
	}
	if mtime("same") != 900 {
		t.Fatal("without a base the client mtime shows")
	}
	b1 := &MtimeBase{CapturedNs: 500, Files: map[string]BaseFile{"same": {"aaaa", 100}, "other": {"xxxx", 40}}}
	w.AddMtimeBase("s1", b1)
	if got := mtime("same"); got != 100 {
		t.Fatalf("unchanged content: %d, want the base's 100", got)
	}
	if got := mtime("other"); got != 501 {
		t.Fatalf("other content with an older mtime: %d, want capture+1", got)
	}
	if got := mtime("new"); got != 950 {
		t.Fatalf("path unknown to the base: %d, want the newer client mtime", got)
	}
	// A second session's base: the newest presentation wins.
	w.AddMtimeBase("s2", &MtimeBase{CapturedNs: 10, Files: map[string]BaseFile{"same": {"aaaa", 300}}})
	if got := mtime("same"); got != 300 {
		t.Fatalf("two bases: %d, want 300", got)
	}
	w.RemoveMtimeBase("s2")
	// Changed and changed back: never the base's mtime again.
	w.Apply([]Node{file("same", "dddd", 1000)}, false, 2)
	w.Apply([]Node{file("same", "aaaa", 1100)}, false, 3)
	if got := mtime("same"); got != 1100 {
		t.Fatalf("reverted content: %d, want the client mtime", got)
	}
	pb := w.PresentedBase()
	if f := pb.Files["same"]; f.MtimeNs != 1100 || f.Digest != "aaaa" {
		t.Fatalf("presented base: %+v", f)
	}
	w.RemoveMtimeBase("s1")
	if mtime("other") != 50 {
		t.Fatal("after the session the client mtime shows")
	}
	var buf bytes.Buffer
	if err := b1.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMtimeBase(&buf)
	if err != nil || got.CapturedNs != 500 || !got.SameFiles(b1) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
