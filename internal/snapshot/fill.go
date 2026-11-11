package snapshot

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Filler restores a snapshot into a local directory: Skeleton creates the
// tree with sparse placeholders, Fill fetches one file's content on demand
// and FillAll fetches the rest in the background.
type Filler struct {
	m       *Manifest
	dir     string // local directory (the live cache)
	st      Store
	chunks  string // directory for fetched chunks
	pending map[string]*Entry

	mu       sync.Mutex
	inflight map[string]chan struct{}
	chunkMu  sync.Mutex
	chunkCh  map[string]chan struct{}
	fetched  atomic.Int64
	filled   atomic.Int64
}

// NewFiller prepares a restore of m into dir; chunks are cached in
// chunkDir until Close.
func NewFiller(m *Manifest, dir string, st Store, chunkDir string) *Filler {
	f := &Filler{m: m, dir: dir, st: st, chunks: chunkDir, pending: map[string]*Entry{},
		inflight: map[string]chan struct{}{}, chunkCh: map[string]chan struct{}{}}
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Mode.IsRegular() && e.Size > 0 {
			f.pending[e.Path] = e
		}
	}
	return f
}

// Skeleton creates directories, symlinks and placeholders: regular files
// with their final size (sparse) and mtime. Directory mtimes are set last.
func (f *Filler) Skeleton() error {
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(f.chunks, 0o755); err != nil {
		return err
	}
	var dirs []*Entry
	for i := range f.m.Entries {
		e := &f.m.Entries[i]
		p := filepath.Join(f.dir, filepath.FromSlash(e.Path))
		switch {
		case e.Mode.IsDir():
			if err := os.MkdirAll(p, e.Mode.Perm()|0o700); err != nil {
				return err
			}
			dirs = append(dirs, e)
		case e.Mode&fs.ModeSymlink != 0:
			os.Remove(p)
			if err := os.Symlink(e.Link, p); err != nil {
				return err
			}
			mt := time.Unix(0, e.MTime)
			lutimes(p, mt)
		case e.Mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			fh, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, e.Mode.Perm()|0o200)
			if err != nil {
				return err
			}
			err = fh.Truncate(e.Size)
			if cerr := fh.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			os.Chmod(p, e.Mode.Perm())
			mt := time.Unix(0, e.MTime)
			if err := os.Chtimes(p, mt, mt); err != nil {
				return err
			}
		}
	}
	// Deepest first, so that creating children does not change a parent's
	// mtime after it was set.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].Path) > len(dirs[j].Path) })
	for _, e := range dirs {
		mt := time.Unix(0, e.MTime)
		os.Chtimes(filepath.Join(f.dir, filepath.FromSlash(e.Path)), mt, mt)
	}
	return nil
}

func lutimes(p string, t time.Time) { _ = lutimesNano(p, t.UnixNano()) }

// Pending reports whether a file's content is not local yet.
func (f *Filler) Pending(rel string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pending[rel]
	return ok
}

// Forget drops a pending file (deleted or replaced locally before it was
// filled).
func (f *Filler) Forget(rel string) {
	f.mu.Lock()
	delete(f.pending, rel)
	f.mu.Unlock()
}

// Remaining is the number of files still pending.
func (f *Filler) Remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// Fill makes a file's content local (no-op if it is not pending).
// Concurrent calls for the same file wait for one fetch.
func (f *Filler) Fill(ctx context.Context, rel string) error {
	f.mu.Lock()
	e, ok := f.pending[rel]
	if !ok {
		f.mu.Unlock()
		return nil
	}
	if ch, busy := f.inflight[rel]; busy {
		f.mu.Unlock()
		select {
		case <-ch:
			if f.Pending(rel) {
				return fmt.Errorf("snapshot: fill %s failed", rel)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ch := make(chan struct{})
	f.inflight[rel] = ch
	f.mu.Unlock()
	err := f.fillEntry(ctx, e)
	f.mu.Lock()
	delete(f.inflight, rel)
	if err == nil {
		delete(f.pending, rel)
		f.filled.Add(1)
	}
	f.mu.Unlock()
	close(ch)
	return err
}

func (f *Filler) fillEntry(ctx context.Context, e *Entry) error {
	p := filepath.Join(f.dir, filepath.FromSlash(e.Path))
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil // replaced or removed locally: nothing to fill
	}
	out, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		os.Chmod(p, fi.Mode().Perm()|0o200)
		if out, err = os.OpenFile(p, os.O_WRONLY, 0); err != nil {
			return err
		}
		defer os.Chmod(p, fi.Mode().Perm())
	}
	var off int64
	for _, r := range e.Refs {
		cp, err := f.chunk(ctx, r.Hash)
		if err != nil {
			out.Close()
			return err
		}
		in, err := os.Open(cp)
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(io.NewOffsetWriter(out, off), io.NewSectionReader(in, r.Off, r.Len))
		in.Close()
		if err != nil {
			out.Close()
			return err
		}
		off += r.Len
	}
	if err := out.Close(); err != nil {
		return err
	}
	mt := time.Unix(0, e.MTime)
	return os.Chtimes(p, mt, mt)
}

// chunk returns the local path of a chunk, fetching it once.
func (f *Filler) chunk(ctx context.Context, hash string) (string, error) {
	p := filepath.Join(f.chunks, hash)
	for {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		f.chunkMu.Lock()
		if ch, busy := f.chunkCh[hash]; busy {
			f.chunkMu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		ch := make(chan struct{})
		f.chunkCh[hash] = ch
		f.chunkMu.Unlock()
		err := f.fetchChunk(ctx, hash, p)
		f.chunkMu.Lock()
		delete(f.chunkCh, hash)
		f.chunkMu.Unlock()
		close(ch)
		if err != nil {
			return "", err
		}
	}
}

func (f *Filler) fetchChunk(ctx context.Context, hash, p string) error {
	rc, _, err := f.st.Get(ctx, ChunkKey(hash))
	if err != nil {
		return fmt.Errorf("chunk %s: %w", hash[:12], err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if hashOf(b) != hash {
		return fmt.Errorf("chunk %s: content hash mismatch", hash[:12])
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	f.fetched.Add(1)
	return os.Rename(tmp, p)
}

// FillAll fills every pending file with parallel workers (0: 8) and
// returns the first error. Files are filled in path order, which keeps
// packed small files of one chunk together.
func (f *Filler) FillAll(ctx context.Context, parallel int) error {
	if parallel <= 0 {
		parallel = 8
	}
	f.mu.Lock()
	paths := make([]string, 0, len(f.pending))
	for p := range f.pending {
		paths = append(paths, p)
	}
	f.mu.Unlock()
	sort.Strings(paths)
	work := make(chan string)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				if err := f.Fill(ctx, p); err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		work <- p
	}
	close(work)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

// Stats reports the restore so far.
func (f *Filler) Stats() Stats {
	return Stats{Bytes: f.m.Bytes, Files: f.filled.Load(), NewChunks: f.fetched.Load()}
}

// Close removes the chunk cache.
func (f *Filler) Close() error { return os.RemoveAll(f.chunks) }
