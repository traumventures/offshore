package anyfs

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source mtimes of a projection and the build directory it feeds.
//
// A projection shows the client's mtimes. mtime-based build tools
// (cargo) compare them with their outputs, so a build directory built
// from one worktree and used by another (a seeded slot), or restored on
// another machine, rebuilds every source whose checkout is newer than the
// outputs although its content is the same; and an older source with
// different content would look fresh. An MtimeBase is the source view a
// build directory was built against: the presented (digest, mtime) of
// every clean file and when it was captured. While a session uses that
// build directory, the projection presents (WritableFS.AddMtimeBase):
//
//   - the base's mtime for a file whose content is the base's content and
//     that has not changed in the projection since the base was added:
//     the outputs were built from exactly that view;
//   - otherwise at least the base's capture time + 1 ns: outputs from
//     other content are older than the capture, so they rebuild.
//
// With several bases (concurrent sessions over different build
// directories) the newest of their presentations wins: a newer mtime
// only ever causes a rebuild, never a stale output.
type MtimeBase struct {
	// CapturedNs is when the view was captured (after the session's
	// builds), UnixNano.
	CapturedNs int64
	// Files maps a path to the presented content and mtime.
	Files map[string]BaseFile
}

// BaseFile is one file of an MtimeBase. Digest is a prefix of the
// content digest (equality with the same path's content only).
type BaseFile struct {
	Digest  string
	MtimeNs int64
}

// baseDigestLen is the stored prefix of a content digest (64 bits:
// compared per path, never across paths).
const baseDigestLen = 16

func baseDigest(d string) string {
	if len(d) > baseDigestLen {
		return d[:baseDigestLen]
	}
	return d
}

// activeBase is a base in use by one session.
type activeBase struct {
	b       *MtimeBase
	changed map[string]bool // paths changed in the projection since added
}

// SameFiles reports whether b and o hold the same files (ignoring the
// capture time).
func (b *MtimeBase) SameFiles(o *MtimeBase) bool {
	if b == nil || o == nil || len(b.Files) != len(o.Files) {
		return false
	}
	for p, f := range b.Files {
		if o.Files[p] != f {
			return false
		}
	}
	return true
}

// Write encodes b (gzip, one "mtime digest path" line per file, sorted).
func (b *MtimeBase) Write(w io.Writer) error {
	zw := gzip.NewWriter(w)
	bw := bufio.NewWriter(zw)
	fmt.Fprintf(bw, "offshore-mtime-base 1 %d\n", b.CapturedNs)
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := b.Files[p]
		fmt.Fprintf(bw, "%d %s %s\n", f.MtimeNs, f.Digest, p)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return zw.Close()
}

// ReadMtimeBase decodes a base written by Write.
func ReadMtimeBase(r io.Reader) (*MtimeBase, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() {
		return nil, fmt.Errorf("mtime base: empty")
	}
	var captured int64
	if _, err := fmt.Sscanf(sc.Text(), "offshore-mtime-base 1 %d", &captured); err != nil {
		return nil, fmt.Errorf("mtime base header: %w", err)
	}
	b := &MtimeBase{CapturedNs: captured, Files: map[string]BaseFile{}}
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("mtime base: bad line %q", sc.Text())
		}
		mt, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("mtime base: %w", err)
		}
		b.Files[parts[2]] = BaseFile{Digest: parts[1], MtimeNs: mt}
	}
	return b, sc.Err()
}

// AddMtimeBase makes the projection present b to the session id (see
// MtimeBase) until RemoveMtimeBase. Paths whose presented mtime changes
// are invalidated in the kernel.
func (w *WritableFS) AddMtimeBase(id string, b *MtimeBase) {
	if b == nil {
		return
	}
	w.mu.Lock()
	before := w.presentedLocked()
	if w.bases == nil {
		w.bases = map[string]*activeBase{}
	}
	w.bases[id] = &activeBase{b: b, changed: map[string]bool{}}
	stale := w.restatedLocked(before)
	w.mu.Unlock()
	w.invalidate(stale, false)
}

// RemoveMtimeBase ends the presentation of the session's base.
func (w *WritableFS) RemoveMtimeBase(id string) {
	w.mu.Lock()
	if _, ok := w.bases[id]; !ok {
		w.mu.Unlock()
		return
	}
	before := w.presentedLocked()
	delete(w.bases, id)
	stale := w.restatedLocked(before)
	w.mu.Unlock()
	w.invalidate(stale, false)
}

// PresentedBase captures the files as presented now: every clean file
// of the tree with a digest (local modifications are left out, so a
// later projection presents them as changed).
func (w *WritableFS) PresentedBase() *MtimeBase {
	w.mu.RLock()
	defer w.mu.RUnlock()
	b := &MtimeBase{CapturedNs: time.Now().UnixNano(), Files: map[string]BaseFile{}}
	for _, n := range w.src.Tree().Nodes() {
		if n.Kind != File || n.Digest == "" {
			continue
		}
		if _, dirty := w.dirty[n.Path]; dirty {
			continue
		}
		b.Files[n.Path] = BaseFile{Digest: baseDigest(n.Digest), MtimeNs: w.mtimeLocked(&n)}
	}
	return b
}

// mtimeLocked is the presented mtime of a clean tree node.
func (w *WritableFS) mtimeLocked(n *Node) int64 {
	if len(w.bases) == 0 || n.Kind != File {
		return n.MtimeNs
	}
	mt := int64(-1 << 63)
	for _, ab := range w.bases {
		v := n.MtimeNs
		f, ok := ab.b.Files[n.Path]
		if ok && !ab.changed[n.Path] && n.Digest != "" && f.Digest == baseDigest(n.Digest) {
			v = f.MtimeNs
		} else if v <= ab.b.CapturedNs {
			v = ab.b.CapturedNs + 1
		}
		mt = max(mt, v)
	}
	return mt
}

// markChangedLocked records paths changed in the projection for every
// active base.
func (w *WritableFS) markChangedLocked(paths []string, full bool) {
	for _, ab := range w.bases {
		if full {
			// A full delta may replace anything: compare with the tree.
			for p, f := range ab.b.Files {
				if n, ok := w.src.Tree().Get(p); !ok || baseDigest(n.Digest) != f.Digest {
					ab.changed[p] = true
				}
			}
		}
		for _, p := range paths {
			ab.changed[p] = true
		}
	}
}

// presentedLocked returns the presented mtimes of the tree's files.
func (w *WritableFS) presentedLocked() map[string]int64 {
	nodes := w.src.Tree().Nodes()
	out := make(map[string]int64, len(nodes))
	for i := range nodes {
		if nodes[i].Kind == File {
			out[nodes[i].Path] = w.mtimeLocked(&nodes[i])
		}
	}
	return out
}

// restatedLocked returns the paths of before whose presented mtime
// differs now.
func (w *WritableFS) restatedLocked(before map[string]int64) []string {
	var out []string
	t := w.src.Tree()
	for p, mt := range before {
		if n, ok := t.Get(p); ok && w.mtimeLocked(n) != mt {
			out = append(out, p)
		}
	}
	return out
}
