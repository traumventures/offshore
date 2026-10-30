// Package manifest describes a projected directory tree: every visible path
// with its type, metadata and content hash, plus versioned deltas.
package manifest

import (
	"encoding/hex"
	"io"
	"os"
	"path"
	"sort"

	"github.com/zeebo/blake3"
)

// Kind is the type of a manifest entry.
type Kind uint8

const (
	File Kind = iota + 1
	Dir
	Symlink
)

func (k Kind) String() string {
	switch k {
	case File:
		return "file"
	case Dir:
		return "dir"
	case Symlink:
		return "symlink"
	}
	return "unknown"
}

// Entry is one path of the tree. Path is relative to the root, uses "/" as
// separator and never starts with "/". The root itself is not an entry.
type Entry struct {
	Path    string `json:"p"`
	Kind    Kind   `json:"k"`
	Mode    uint32 `json:"m,omitempty"` // permission bits
	Size    int64  `json:"s,omitempty"`
	MtimeNs int64  `json:"t,omitempty"`
	Hash    string `json:"h,omitempty"` // BLAKE3 of the content, files only
	Target  string `json:"l,omitempty"` // symlink target
}

// SameContent reports whether two entries describe the same object.
func (e Entry) SameContent(o Entry) bool {
	return e.Kind == o.Kind && e.Mode == o.Mode && e.Hash == o.Hash && e.Target == o.Target && e.Size == o.Size
}

// Same reports whether two entries are identical including timestamps.
func (e Entry) Same(o Entry) bool {
	return e.SameContent(o) && e.MtimeNs == o.MtimeNs
}

// Delta transforms one manifest version into the next.
type Delta struct {
	Version uint64   `json:"v"`
	Full    bool     `json:"full,omitempty"` // replace everything instead of patching
	Upserts []Entry  `json:"u,omitempty"`
	Deletes []string `json:"d,omitempty"` // deleting a directory deletes its subtree
}

// Empty reports whether the delta changes nothing.
func (d Delta) Empty() bool { return !d.Full && len(d.Upserts) == 0 && len(d.Deletes) == 0 }

// Tree is a set of entries keyed by path.
type Tree map[string]Entry

// Clone returns a shallow copy.
func (t Tree) Clone() Tree {
	out := make(Tree, len(t))
	for k, v := range t {
		out[k] = v
	}
	return out
}

// Apply applies a delta in place.
func (t Tree) Apply(d Delta) {
	if d.Full {
		for k := range t {
			delete(t, k)
		}
	}
	for _, p := range d.Deletes {
		t.DeleteSubtree(p)
	}
	for _, e := range d.Upserts {
		if old, ok := t[e.Path]; ok && old.Kind == Dir && e.Kind != Dir {
			t.DeleteSubtree(e.Path)
		}
		t[e.Path] = e
	}
}

// DeleteSubtree removes p and everything below it.
func (t Tree) DeleteSubtree(p string) {
	delete(t, p)
	prefix := p + "/"
	for k := range t {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(t, k)
		}
	}
}

// Diff returns the delta that turns old into new. Deletes are minimal: when a
// directory disappears only the directory is listed.
func Diff(old, new Tree) Delta {
	var d Delta
	for p, e := range new {
		if o, ok := old[p]; !ok || !o.Same(e) {
			d.Upserts = append(d.Upserts, e)
		}
	}
	for p := range old {
		if _, ok := new[p]; ok {
			continue
		}
		if parent := path.Dir(p); parent != "." {
			if _, gone := old[parent]; gone {
				if _, still := new[parent]; !still {
					continue // covered by deleting the parent
				}
			}
		}
		d.Deletes = append(d.Deletes, p)
	}
	sort.Slice(d.Upserts, func(i, j int) bool { return d.Upserts[i].Path < d.Upserts[j].Path })
	sort.Strings(d.Deletes)
	return d
}

// Entries returns all entries sorted by path (parents before children).
func (t Tree) Entries() []Entry {
	out := make([]Entry, 0, len(t))
	for _, e := range t {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// HashFile returns the hex BLAKE3 hash and size of a file.
func HashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return HashReader(f)
}

// HashReader hashes everything read from r.
func HashReader(r io.Reader) (string, int64, error) {
	h := blake3.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashBytes hashes b.
func HashBytes(b []byte) string {
	s := blake3.Sum256(b)
	return hex.EncodeToString(s[:])
}
