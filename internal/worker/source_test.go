package worker

import (
	"testing"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/manifest"
)

// A manifest delta applied as an anyfs layer gives the tree that
// manifest.Tree.Apply gives.
func TestDeltaLayerMatchesManifestApply(t *testing.T) {
	base := manifest.Tree{
		"a":   {Path: "a", Kind: manifest.Dir, Mode: 0o755},
		"a/x": {Path: "a/x", Kind: manifest.File, Mode: 0o644, Hash: "h1", Size: 1},
		"b":   {Path: "b", Kind: manifest.Dir, Mode: 0o755},
		"b/y": {Path: "b/y", Kind: manifest.File, Mode: 0o644, Hash: "h2", Size: 1},
		"c":   {Path: "c", Kind: manifest.File, Mode: 0o644, Hash: "h3", Size: 1},
	}
	d := manifest.Delta{
		Deletes: []string{"a", "c"},
		Upserts: []manifest.Entry{
			{Path: "a", Kind: manifest.Dir, Mode: 0o700},
			{Path: "a/z", Kind: manifest.File, Mode: 0o644, Hash: "h4", Size: 2},
			{Path: "b", Kind: manifest.File, Mode: 0o644, Hash: "h5", Size: 3},
		},
	}
	var nodes []anyfs.Node
	for _, e := range base.Entries() {
		nodes = append(nodes, anyfs.FromEntry(e))
	}
	got := anyfs.Apply(anyfs.NewTree(nodes), deltaLayer(d))
	want := base.Clone()
	want.Apply(d)
	if got.Len() != len(want) {
		t.Fatalf("got %d nodes %v, want %v", got.Len(), got.Nodes(), want)
	}
	for _, e := range want {
		n, ok := got.Get(e.Path)
		if !ok || n.Entry() != e {
			t.Errorf("%s: got %+v, want %+v", e.Path, n, e)
		}
	}
}
