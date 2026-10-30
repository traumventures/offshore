package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genTree generates a consistent tree (every entry's parent is a directory).
func genTree(t *rapid.T, label string) Tree {
	tree := Tree{}
	names := []string{"a", "b", "c", "d"}
	var dirs = []string{""}
	n := rapid.IntRange(0, 25).Draw(t, label+"-n")
	for i := 0; i < n; i++ {
		parent := rapid.SampledFrom(dirs).Draw(t, label+"-parent")
		name := rapid.SampledFrom(names).Draw(t, label+"-name")
		p := name
		if parent != "" {
			p = parent + "/" + name
		}
		if _, ok := tree[p]; ok {
			continue
		}
		switch rapid.IntRange(0, 2).Draw(t, label+"-kind") {
		case 0:
			tree[p] = Entry{Path: p, Kind: Dir, Mode: 0o755}
			dirs = append(dirs, p)
		case 1:
			h := rapid.SampledFrom([]string{"h1", "h2", "h3"}).Draw(t, label+"-hash")
			tree[p] = Entry{Path: p, Kind: File, Mode: 0o644, Hash: h, Size: int64(len(h)), MtimeNs: rapid.Int64Range(1, 3).Draw(t, label+"-mt")}
		case 2:
			tree[p] = Entry{Path: p, Kind: Symlink, Target: "x", Size: 1}
		}
	}
	return tree
}

func TestDiffApplyRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genTree(t, "a")
		b := genTree(t, "b")
		d := Diff(a, b)
		got := a.Clone()
		got.Apply(d)
		if !reflect.DeepEqual(got, b) {
			t.Fatalf("apply(diff(a,b)) != b\na=%v\nb=%v\ndelta=%+v\ngot=%v", a, b, d, got)
		}
		if len(Diff(b, b).Upserts)+len(Diff(b, b).Deletes) != 0 {
			t.Fatalf("diff of identical trees is not empty")
		}
	})
}

// TestRescanMatchesScan mutates a real directory randomly and checks that
// incremental rescans of the touched directories always equal a full scan.
func TestRescanMatchesScan(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		root, err := os.MkdirTemp("", "manifest-test-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(root)
		s := NewScanner(root, Rules{Gitignore: true, Hidden: []string{"target"}})
		tree, err := s.Scan(nil)
		if err != nil {
			t.Fatal(err)
		}
		dirs := []string{""}
		steps := rapid.IntRange(1, 30).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			dir := rapid.SampledFrom(dirs).Draw(t, "dir")
			name := rapid.SampledFrom([]string{"x", "y", "z", "target", ".gitignore", "ign"}).Draw(t, "name")
			rel := name
			if dir != "" {
				rel = dir + "/" + name
			}
			abs := filepath.Join(root, filepath.FromSlash(rel))
			var touched []string
			switch rapid.IntRange(0, 4).Draw(t, "op") {
			case 0: // write file
				os.RemoveAll(abs)
				content := "ign\n"
				if name != ".gitignore" {
					content = fmt.Sprint(rapid.IntRange(0, 3).Draw(t, "content"))
				}
				os.WriteFile(abs, []byte(content), 0o644)
				touched = []string{dir, rel} // like the watcher: parent + created path
			case 1: // mkdir
				os.RemoveAll(abs)
				if os.Mkdir(abs, 0o755) == nil {
					dirs = append(dirs, rel)
				}
				touched = []string{dir, rel}
			case 2: // remove
				os.RemoveAll(abs)
				touched = []string{dir}
			case 3: // symlink
				os.RemoveAll(abs)
				os.Symlink("target-of-"+name, abs)
				touched = []string{dir, rel}
			case 4: // chmod
				os.Chmod(abs, 0o600)
				touched = []string{dir}
			}
			// Drop directories that no longer exist from the candidates.
			live := dirs[:0]
			for _, d := range dirs {
				if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(d))); d == "" || (err == nil && fi.IsDir()) {
					live = append(live, d)
				}
			}
			dirs = live
			full := name == ".gitignore"
			var d Delta
			if full {
				next, err := s.Scan(tree)
				if err != nil {
					t.Fatal(err)
				}
				d = Diff(tree, next)
			} else if d, err = s.Rescan(tree, touched); err != nil {
				t.Fatal(err)
			}
			tree.Apply(d)
			want, err := s.Scan(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stripTimes(tree), stripTimes(want)) {
				t.Fatalf("step %d (%s): incremental tree differs from full scan\nincremental=%v\nfull=%v", i, rel, keys(tree), keys(want))
			}
		}
	})
}

func stripTimes(t Tree) Tree {
	out := Tree{}
	for k, v := range t {
		v.MtimeNs = 0
		out[k] = v
	}
	return out
}

func keys(t Tree) string {
	var ks []string
	for _, e := range t.Entries() {
		ks = append(ks, e.Path+":"+e.Kind.String())
	}
	return strings.Join(ks, ",")
}

func TestModeOf(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n/build/\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", ".gitignore"), []byte("secret\n!keep.log\n"), 0o644)
	s := NewScanner(root, Rules{
		Gitignore: true,
		Hidden:    []string{"target"},
		Rules: []Rule{
			{Patterns: []string{"vendor/**"}, Mode: ModeImmutable},
			{Patterns: []string{"docs/private/**"}, Mode: ModeHidden},
		},
	})
	if _, err := s.Scan(nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel   string
		isDir bool
		want  Mode
	}{
		{"src/main.rs", false, ModeWatch},
		{"x.log", false, ModeHidden},
		{"sub/x.log", false, ModeHidden},
		{"sub/keep.log", false, ModeWatch},
		{"sub/secret", false, ModeHidden},
		{"build", true, ModeHidden},
		{"build/out", false, ModeHidden},
		{"target", true, ModeHidden},
		{"target/debug/x", false, ModeHidden},
		{".git", true, ModeHidden},
		{"vendor", true, ModeImmutable},
		{"vendor/a/b.rs", false, ModeImmutable},
		{"docs/private/x", false, ModeHidden},
		{"docs/public", false, ModeWatch},
	}
	for _, c := range cases {
		if got := s.ModeOf(c.rel, c.isDir); got != c.want {
			t.Errorf("ModeOf(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}
