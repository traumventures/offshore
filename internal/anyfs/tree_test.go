package anyfs

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func f(p string, mt int64, digest string) Node {
	return Node{Path: p, Kind: File, Mode: 0o644, Size: int64(len(digest)), MtimeNs: mt, Digest: digest}
}
func d(p string) Node { return Node{Path: p, Kind: Dir, Mode: 0o755} }

func paths(t *Tree) []string {
	var out []string
	for _, n := range t.Nodes() {
		out = append(out, n.Path)
	}
	return out
}

func TestTreeOrderAndChildren(t *testing.T) {
	tr := NewTree([]Node{f("a/b", 1, "x"), d("a"), f("a-b", 1, "y"), d("a/c"), f("a/c/d", 1, "z")})
	if got, want := paths(tr), []string{"a", "a/b", "a/c", "a/c/d", "a-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if k := tr.Children("a"); len(k) != 2 || k[0].Path != "a/b" || k[1].Path != "a/c" {
		t.Fatalf("children of a: %v", k)
	}
	if tr.Bytes() != 3 {
		t.Fatalf("bytes %d", tr.Bytes())
	}
}

func TestDiffApplyCases(t *testing.T) {
	base := NewTree([]Node{d("d"), f("d/x", 1, "x"), f("d/y", 1, "y"), d("e"), f("e/z", 1, "z"), f("g", 1, "g")})
	top := NewTree([]Node{
		f("d/x", 2, "x2"), d("d"), // changed file
		f("e", 3, "file"),         // directory replaced by a file
		d("g"), f("g/h", 1, "h"),  // file replaced by a directory
		f("n", 1, "new"),
	})
	layer := Diff(base, top)
	got := Apply(base, layer)
	if !reflect.DeepEqual(paths(got), paths(top)) {
		t.Fatalf("apply(diff) paths %v, want %v (layer %+v)", paths(got), paths(top), layer)
	}
	for _, n := range top.Nodes() {
		g, _ := got.Get(n.Path)
		if !g.Same(&n) {
			t.Fatalf("%s: got %+v want %+v", n.Path, g, n)
		}
	}
	wh := 0
	for _, n := range layer {
		if n.Whiteout {
			wh++
			if n.Path != "d/y" {
				t.Fatalf("unexpected whiteout %s", n.Path)
			}
		}
		if n.Path == "g" && !n.Opaque {
			t.Fatal("directory replacing a file must be opaque")
		}
	}
	if wh != 1 {
		t.Fatalf("whiteouts %d", wh)
	}
}

// randomTree builds a random tree of depth <= 3 over a small name space.
func randomTree(r *rand.Rand) *Tree {
	var ns []Node
	var add func(dir string, depth int)
	add = func(dir string, depth int) {
		for i := 0; i < r.Intn(4); i++ {
			name := fmt.Sprintf("n%d", r.Intn(5))
			p := name
			if dir != "" {
				p = dir + "/" + name
			}
			if depth < 3 && r.Intn(3) == 0 {
				ns = append(ns, d(p))
				add(p, depth+1)
			} else if r.Intn(5) == 0 {
				ns = append(ns, Node{Path: p, Kind: Symlink, Target: fmt.Sprint(r.Intn(3))})
			} else {
				ns = append(ns, f(p, int64(r.Intn(3)), fmt.Sprint(r.Intn(3))))
			}
		}
	}
	add("", 0)
	// Drop nodes whose parent is not a directory (duplicates by name).
	t := NewTree(ns)
	var ok []Node
	dirs := map[string]bool{"": true}
	for _, n := range t.Nodes() {
		if dirs[parent(n.Path)] {
			ok = append(ok, n)
			if n.IsDir() {
				dirs[n.Path] = true
			}
		}
	}
	return NewTree(ok)
}

func TestDiffApplyProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		a, b, c := randomTree(r), randomTree(r), randomTree(r)
		// Two layers on top of each other fold to the top tree.
		got := Merge(Diff(NewTree(nil), a), Diff(a, b), Diff(b, c))
		if !reflect.DeepEqual(paths(got), paths(c)) {
			t.Fatalf("iteration %d: %v, want %v", i, paths(got), paths(c))
		}
		for _, n := range c.Nodes() {
			g, _ := got.Get(n.Path)
			if !g.Same(&n) {
				t.Fatalf("iteration %d %s: %+v want %+v", i, n.Path, g, n)
			}
		}
	}
}

func TestMergeNewest(t *testing.T) {
	local := NewTree([]Node{d("debug"), f("debug/a", 5, "la"), f("debug/b", 1, "lb")})
	remote := NewTree([]Node{d("debug"), f("debug/a", 3, "ra"), f("debug/b", 2, "rb"), f("debug/c", 1, "rc")})
	m := MergeNewest(local, remote)
	want := map[string]string{"debug/a": "la", "debug/b": "rb", "debug/c": "rc"}
	for p, dg := range want {
		if n, ok := m.Get(p); !ok || n.Digest != dg {
			t.Fatalf("%s: %+v want digest %s", p, n, dg)
		}
	}
}

func TestEncodeDecode(t *testing.T) {
	l := &Layer{ID: "1-a", Nodes: []Node{f("a", 1, "x"), {Path: "b", Whiteout: true}}}
	var buf bytes.Buffer
	if err := Encode(&buf, l); err != nil {
		t.Fatal(err)
	}
	var out Layer
	if err := Decode(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Nodes, out.Nodes) {
		t.Fatalf("%+v", out)
	}
}
