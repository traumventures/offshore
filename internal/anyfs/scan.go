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
	// FileSink, if set, returns the sink of the pages of one large file
	// (files of their own pages; e.g. streamed to the store by the
	// file's access class); small files packed together go to Sink.
	FileSink func(rel string) PageSink
	// Workers bounds the files read concurrently (0: DefaultReadWorkers).
	// Small files are still packed in path order, so pages do not depend
	// on it; the sink must accept concurrent puts.
	Workers int
}

// DefaultReadWorkers is the default number of files read concurrently:
// enough to keep a network volume's queue busy and several cores
// hashing, bounded so a scan does not starve the build's IO.
const DefaultReadWorkers = 4

// ScanStats describe a scan.
type ScanStats struct {
	Files     int64 // regular files
	Bytes     int64 // their size
	Read      int64 // bytes read (changed files)
	Hashed    int64 // files read (changed files)
	Reused    int64 // files reused from Prev
	Known     int64 // changed files whose digest was known (hashed in the background)
	KnownRead int64 // their bytes (not read by the scan)
	Examined  int64 // entries of the scanned directory stat'ed
	Pages     int64 // pages emitted to the sink
	PageBytes int64
}

// scanned is a node found by a walk with its ctime.
type scanned struct {
	Node
	ctime int64
	// meta: a metadata-only entry (overlay metacopy): the content is the
	// lower file's at from ("": the same path) or of the content digest
	// fromDigest; a fresh ctime, mode or mtime does not mean new content.
	meta       bool
	from       string
	fromDigest string
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

// reusable reports whether n can take the digest and pages of Prev (of
// lower or byDigest for metadata-only entries).
func reusable(n *scanned, opt ScanOptions, paged bool, lower *Tree, byDigest map[string]*Node) (*Node, bool) {
	if n.meta {
		return metaSource(n, opt.Prev, paged, lower, byDigest)
	}
	p, ok := opt.Prev.Get(n.Path)
	if !ok || p.Kind != File || (p.Mode != n.Mode && !n.meta) || p.Size != n.Size || p.MtimeNs != n.MtimeNs || p.Digest == "" {
		return nil, false
	}
	if paged && len(p.Pages) == 0 && p.Size != 0 {
		return nil, false
	}
	if opt.Since > 0 && n.ctime >= opt.Since {
		return nil, false
	}
	return p, true
}

// metaSource is the node whose content a metadata-only entry has: the
// lower file it redirects to (by path or content digest), if its size
// matches.
func metaSource(n *scanned, prev *Tree, paged bool, lower *Tree, byDigest map[string]*Node) (*Node, bool) {
	var p *Node
	if n.fromDigest != "" {
		p = byDigest[n.fromDigest]
	} else {
		from := n.Path
		if n.from != "" {
			from = n.from
		}
		t := lower
		if t == nil {
			t = prev
		}
		if t != nil {
			p, _ = t.Get(from)
		}
	}
	if p == nil || p.Kind != File || p.Size != n.Size || p.Digest == "" || (paged && len(p.Pages) == 0 && p.Size != 0) {
		return nil, false
	}
	return p, true
}

// digestIndex maps the content digests of the trees' files to a node
// holding them (for metadata-only entries redirecting by digest).
func digestIndex(paged bool, trees ...*Tree) map[string]*Node {
	out := map[string]*Node{}
	for _, t := range trees {
		if t == nil {
			continue
		}
		ns := t.Nodes()
		for i := range ns {
			n := &ns[i]
			if n.Kind != File || n.Digest == "" || (paged && len(n.Pages) == 0 && n.Size != 0) {
				continue
			}
			if _, ok := out[n.Digest]; !ok {
				out[n.Digest] = n
			}
		}
	}
	return out
}

// hashNodes sets digest and pages of the files among nodes: reused from
// opt.Prev when unchanged, else read (and paged into opt.Sink). Files
// removed meanwhile get Kind 0.
func hashNodes(dir string, nodes []*scanned, opt ScanOptions, st *ScanStats) error {
	return readFiles(dir, reuseNodes(nodes, opt, nil, st), opt, st)
}

// reuseNodes takes digest and pages of unchanged files from opt.Prev
// (metadata-only entries from lower) and returns the files that must be
// read.
func reuseNodes(nodes []*scanned, opt ScanOptions, lower *Tree, st *ScanStats) []*Node {
	var read []*Node
	var byDigest map[string]*Node
	for _, n := range nodes {
		if n.fromDigest != "" {
			byDigest = digestIndex(opt.Sink != nil, lower, opt.Prev)
			break
		}
	}
	for _, n := range nodes {
		if n.Kind != File {
			continue
		}
		if p, ok := reusable(n, opt, opt.Sink != nil, lower, byDigest); ok {
			n.Digest, n.Pages = p.Digest, p.Pages
			st.Reused++
			continue
		}
		read = append(read, &n.Node)
	}
	return read
}

// readFiles reads the files below dir into their nodes (digest, pages to
// opt.Sink); files gone meanwhile get Kind 0. Up to opt.Workers files are
// read at once: large files are hashed and paged by the reader, small
// files are packed by one consumer in the order of nodes.
func readFiles(dir string, nodes []*Node, opt ScanOptions, st *ScanStats) error {
	workers := opt.Workers
	if workers <= 0 {
		workers = DefaultReadWorkers
	}
	var pk *Packer
	if opt.Sink != nil {
		pk = NewPacker(opt.Sink)
	}
	type job struct {
		n      *Node
		small  []byte // content of a small file to pack
		packed bool   // small: packed by the consumer
		alone  bool
		pages  int64 // pages emitted by the reader
		pbytes int64
		err    error
		done   chan struct{}
	}
	// ordered bounds the jobs in flight (and the small contents held).
	ordered := make(chan *job, workers*8)
	sem := make(chan struct{}, workers)
	stop := make(chan struct{})
	go func() {
		defer close(ordered)
		for _, n := range nodes {
			j := &job{n: n, done: make(chan struct{}), alone: opt.Alone != nil && opt.Alone(n.Path, n.Size)}
			select {
			case ordered <- j:
			case <-stop:
				return
			}
			select {
			case sem <- struct{}{}:
			case <-stop:
				close(j.done)
				return
			}
			go func() {
				defer func() { <-sem; close(j.done) }()
				path := filepath.Join(dir, filepath.FromSlash(j.n.Path))
				if pk != nil && j.n.Size <= SmallFile {
					b, err := os.ReadFile(path)
					if err == nil {
						j.n.Size, j.n.Digest = int64(len(b)), string(KeyOf(b))
					}
					j.small, j.packed, j.err = b, true, err
					return
				}
				var lp *Packer
				if pk != nil {
					sink := opt.Sink
					if opt.FileSink != nil {
						sink = opt.FileSink(j.n.Path)
					}
					lp = NewPacker(sink)
				}
				j.err = readNode(j.n, path, lp, false)
				if lp != nil {
					j.pages, j.pbytes = lp.Pages()
				}
			}()
		}
	}()
	var firstErr error
	var pages, pbytes int64
	for j := range ordered {
		<-j.done
		if firstErr != nil {
			continue
		}
		err := j.err
		if err == nil && j.packed {
			if j.alone {
				err = pk.addAlone(j.n, j.small)
			} else {
				err = pk.addSmall(j.n, j.small)
			}
		}
		if err != nil {
			if os.IsNotExist(err) {
				j.n.Kind = 0 // removed while scanning
				continue
			}
			firstErr = err
			close(stop)
			continue
		}
		pages += j.pages
		pbytes += j.pbytes
		st.Read += j.n.Size
		st.Hashed++
	}
	if firstErr != nil {
		return firstErr
	}
	if pk != nil {
		if err := pk.Flush(); err != nil {
			return err
		}
		p, b := pk.Pages()
		st.Pages, st.PageBytes = st.Pages+p+pages, st.PageBytes+b+pbytes
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
