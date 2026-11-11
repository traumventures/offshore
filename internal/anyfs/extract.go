package anyfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Extract writes a tree below dir (created if missing) with the content
// of its files read from the pages of tier: an eager restore for hosts
// without FUSE and for immutable caches. Existing entries are replaced.
func Extract(ctx context.Context, tier Tier, t *Tree, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var last Key
	var lastB []byte
	page := func(k Key) ([]byte, error) {
		if k == last {
			return lastB, nil
		}
		b, err := tier.Get(ctx, k)
		if err == nil {
			last, lastB = k, b
		}
		return b, err
	}
	nodes := t.Nodes()
	for i := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := &nodes[i]
		p := filepath.Join(dir, filepath.FromSlash(n.Path))
		switch n.Kind {
		case Dir:
			if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
				os.RemoveAll(p)
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case Symlink:
			os.RemoveAll(p)
			if err := os.Symlink(n.Target, p); err != nil {
				return err
			}
		case File:
			os.RemoveAll(p)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			var off int64
			for _, r := range n.Pages {
				b, err := page(r.Key)
				if err == nil && r.Off+r.Len > int64(len(b)) {
					err = fmt.Errorf("page %s too short for %s", r.Key.Short(), n.Path)
				}
				if err == nil {
					_, err = f.WriteAt(b[r.Off:r.Off+r.Len], off)
				}
				if err != nil {
					f.Close()
					return fmt.Errorf("extract %s: %w", n.Path, err)
				}
				off += r.Len
			}
			if err := f.Close(); err != nil {
				return err
			}
			if off != n.Size {
				return fmt.Errorf("extract %s: pages hold %d of %d bytes", n.Path, off, n.Size)
			}
		}
	}
	// Modes and mtimes last, deepest first (writing a child changes its
	// directory's mtime).
	for i := len(nodes) - 1; i >= 0; i-- {
		n := &nodes[i]
		if n.Kind == Symlink {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(n.Path))
		mt := time.Unix(0, n.MtimeNs)
		os.Chmod(p, n.FileMode().Perm())
		os.Chtimes(p, mt, mt)
	}
	return nil
}
