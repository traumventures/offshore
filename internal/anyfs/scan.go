package anyfs

import (
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"
)

// ScanOptions tune Scan.
type ScanOptions struct {
	// Prev is a tree of the same directory (e.g. the lower layers): files
	// with the same kind, mode, size and mtime reuse its digest and pages
	// instead of being read.
	Prev *Tree
	// Sink receives the pages of files that are read. Nil: files are
	// hashed but not paged (Pages stay empty).
	Sink PageSink
	// Skip excludes a path (and, for directories, everything below it).
	Skip func(rel string, d fs.DirEntry) bool
}

// ScanStats describe a scan.
type ScanStats struct {
	Files     int64 // regular files
	Bytes     int64 // their size
	Read      int64 // bytes read (changed files)
	Reused    int64 // files reused from Prev
	Pages     int64 // pages emitted to the sink
	PageBytes int64
}

// Scan builds the tree of a local directory. Sockets and devices are
// skipped (overlayfs whiteouts are character devices: scan the merged
// view, not an upper directory).
func Scan(dir string, opt ScanOptions) (*Tree, ScanStats, error) {
	var nodes []Node
	var st ScanStats
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != dir && os.IsNotExist(err) {
				return nil // removed while walking
			}
			return err
		}
		if p == dir {
			return nil
		}
		rel := filepath.ToSlash(p[len(dir)+1:])
		if opt.Skip != nil && opt.Skip(rel, d) {
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
		n := Node{Path: rel, Mode: uint32(fi.Mode().Perm()), MtimeNs: fi.ModTime().UnixNano()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			n.Kind = Symlink
			if n.Target, err = os.Readlink(p); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			n.Kind = File
			n.Size = fi.Size()
		case fi.IsDir():
			n.Kind = Dir
		default:
			return nil
		}
		nodes = append(nodes, n)
		return nil
	})
	if err != nil {
		return nil, st, err
	}
	var pk *Packer
	if opt.Sink != nil {
		pk = NewPacker(opt.Sink)
	}
	for i := range nodes {
		n := &nodes[i]
		if n.Kind != File {
			continue
		}
		st.Files++
		st.Bytes += n.Size
		if p, ok := opt.Prev.Get(n.Path); ok && p.Kind == File && p.Mode == n.Mode && p.Size == n.Size &&
			p.MtimeNs == n.MtimeNs && p.Digest != "" && (pk == nil || len(p.Pages) > 0 || p.Size == 0) {
			n.Digest, n.Pages = p.Digest, p.Pages
			st.Reused++
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(n.Path))
		if err := readNode(n, path, pk); err != nil {
			if os.IsNotExist(err) {
				n.Kind = 0 // removed while scanning
				continue
			}
			return nil, st, err
		}
		st.Read += n.Size
	}
	if pk != nil {
		if err := pk.Flush(); err != nil {
			return nil, st, err
		}
		st.Pages, st.PageBytes = pk.Pages()
	}
	out := nodes[:0]
	for _, n := range nodes {
		if n.Kind != 0 {
			out = append(out, n)
		}
	}
	return NewTree(out), st, nil
}

// readNode reads a file's content into n (digest, and pages with pk).
func readNode(n *Node, path string, pk *Packer) error {
	if pk != nil && n.Size <= SmallFile {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
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
