package anyfs

import (
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zeebo/blake3"
)

// ScanOptions tune Scan.
type ScanOptions struct {
	// Prev is a tree of the same directory (e.g. the lower layers): files
	// with the same kind, mode, size and mtime reuse its digest and pages
	// instead of being read.
	Prev *Tree
	// Since (unix ns, 0: unknown) is the watermark of Prev: files whose
	// ctime is at or after it changed since and are read even when size
	// and mtime match (content rewritten with a preserved mtime).
	Since int64
	// Sink receives the pages of files that are read. Nil: files are
	// hashed but not paged (Pages stay empty).
	Sink PageSink
	// Alone reports files that get pages of their own instead of being
	// packed with others (their page key is their content digest, so the
	// same content in another slot or repository is the same page:
	// dedupe). Nil: small files are packed.
	Alone func(rel string, size int64) bool
	// Skip excludes a path (and, for directories, everything below it).
	Skip func(rel string, d fs.DirEntry) bool
}

// ScanStats describe a scan.
type ScanStats struct {
	Files     int64 // regular files
	Bytes     int64 // their size
	Read      int64 // bytes read (changed files)
	Hashed    int64 // files read (changed files)
	Reused    int64 // files reused from Prev
	Examined  int64 // entries of the scanned directory stat'ed
	Pages     int64 // pages emitted to the sink
	PageBytes int64
}

// scanned is a node found by a walk with its ctime.
type scanned struct {
	Node
	ctime int64
	// meta: a metadata-only copy-up (overlay metacopy): the content is
	// the lower file's, a fresh ctime does not mean new content.
	meta bool
}

// ctimeOf returns a file's change time (unix ns, 0 if unknown).
func ctimeOf(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return 0
}

// scanNode builds the node of a directory entry (ok false: not a kind a
// tree holds, e.g. a socket or a whiteout device).
func scanNode(p, rel string, fi fs.FileInfo) (scanned, bool, error) {
	n := scanned{Node: Node{Path: rel, Mode: uint32(fi.Mode().Perm()), MtimeNs: fi.ModTime().UnixNano()}, ctime: ctimeOf(fi)}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		n.Kind = Symlink
		t, err := os.Readlink(p)
		if err != nil {
			return n, false, err
		}
		n.Target = t
	case fi.Mode().IsRegular():
		n.Kind = File
		n.Size = fi.Size()
	case fi.IsDir():
		n.Kind = Dir
	default:
		return n, false, nil
	}
	return n, true, nil
}

// walk collects the nodes below dir/rel (rel itself included unless it is
// the root ""); paths are relative to dir.
func walk(dir, rel string, skip func(string, fs.DirEntry) bool, st *ScanStats) ([]scanned, error) {
	var out []scanned
	root := dir
	if rel != "" {
		root = filepath.Join(dir, filepath.FromSlash(rel))
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != dir && os.IsNotExist(err) {
				return nil // removed while walking
			}
			return err
		}
		if p == dir {
			return nil
		}
		r := filepath.ToSlash(p[len(dir)+1:])
		if skip != nil && skip(r, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		st.Examined++
		n, ok, err := scanNode(p, r, fi)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if ok {
			out = append(out, n)
		}
		return nil
	})
	return out, err
}

// Scan builds the tree of a local directory. Sockets and devices are
// skipped (overlayfs whiteouts are character devices: scan the merged
// view, not an upper directory; ScanChanges reads an upper directory to
// find what changed).
func Scan(dir string, opt ScanOptions) (*Tree, ScanStats, error) {
	var st ScanStats
	nodes, err := walk(dir, "", opt.Skip, &st)
	if err != nil {
		return nil, st, err
	}
	ps := make([]*scanned, len(nodes))
	for i := range nodes {
		ps[i] = &nodes[i]
	}
	if err := hashNodes(dir, ps, opt, &st); err != nil {
		return nil, st, err
	}
	return buildTree(nodes, &st), st, nil
}

// buildTree makes the tree of the nodes still present (Kind 0: removed
// while scanning) and counts its files.
func buildTree(nodes []scanned, st *ScanStats) *Tree {
	out := make([]Node, 0, len(nodes))
	st.Files, st.Bytes = 0, 0
	for _, n := range nodes {
		if n.Kind == 0 {
			continue
		}
		if n.Kind == File {
			st.Files++
			st.Bytes += n.Size
		}
		out = append(out, n.Node)
	}
	return NewTree(out)
}

// reusable reports whether n can take the digest and pages of Prev.
func reusable(n *scanned, opt ScanOptions, paged bool) (*Node, bool) {
	p, ok := opt.Prev.Get(n.Path)
	if !ok || p.Kind != File || (p.Mode != n.Mode && !n.meta) || p.Size != n.Size || p.MtimeNs != n.MtimeNs || p.Digest == "" {
		return nil, false
	}
	if paged && len(p.Pages) == 0 && p.Size != 0 {
		return nil, false
	}
	if opt.Since > 0 && n.ctime >= opt.Since && !n.meta {
		return nil, false
	}
	return p, true
}

// hashNodes sets digest and pages of the files among nodes: reused from
// opt.Prev when unchanged, else read (and paged into opt.Sink). Files
// removed meanwhile get Kind 0.
func hashNodes(dir string, nodes []*scanned, opt ScanOptions, st *ScanStats) error {
	return readFiles(dir, reuseNodes(nodes, opt, st), opt, st)
}

// reuseNodes takes digest and pages of unchanged files from opt.Prev and
// returns the files that must be read.
func reuseNodes(nodes []*scanned, opt ScanOptions, st *ScanStats) []*Node {
	var read []*Node
	for _, n := range nodes {
		if n.Kind != File {
			continue
		}
		if p, ok := reusable(n, opt, opt.Sink != nil); ok {
			n.Digest, n.Pages = p.Digest, p.Pages
			st.Reused++
			continue
		}
		read = append(read, &n.Node)
	}
	return read
}

// readFiles reads the files below dir into their nodes (digest, pages to
// opt.Sink); files gone meanwhile get Kind 0.
func readFiles(dir string, nodes []*Node, opt ScanOptions, st *ScanStats) error {
	var pk *Packer
	if opt.Sink != nil {
		pk = NewPacker(opt.Sink)
	}
	for _, n := range nodes {
		path := filepath.Join(dir, filepath.FromSlash(n.Path))
		alone := opt.Alone != nil && opt.Alone(n.Path, n.Size)
		if err := readNode(n, path, pk, alone); err != nil {
			if os.IsNotExist(err) {
				n.Kind = 0 // removed while scanning
				continue
			}
			return err
		}
		st.Read += n.Size
		st.Hashed++
	}
	if pk != nil {
		if err := pk.Flush(); err != nil {
			return err
		}
		st.Pages, st.PageBytes = pk.Pages()
	}
	return nil
}

// readNode reads a file's content into n (digest, and pages with pk;
// alone: a page of its own).
func readNode(n *Node, path string, pk *Packer, alone bool) error {
	if pk != nil && n.Size <= SmallFile {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if alone {
			return pk.AddAlone(n, b)
		}
		return pk.AddSmall(n, b)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if pk != nil {
		return pk.AddLarge(n, f)
	}
	h := blake3.New()
	sz, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	n.Size, n.Digest = sz, hex.EncodeToString(h.Sum(nil))
	return nil
}
