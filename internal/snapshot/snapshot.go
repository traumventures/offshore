// Package snapshot saves directory trees (build slots, machine caches) as
// content addressed chunks into a store and restores them lazily.
//
// A snapshot is a manifest (snapshots/<unix seconds>.json.gz, the newest
// named by snapshots/latest) listing every file with mode, size, mtime and
// the chunk ranges holding its content. Chunks live below chunks/ keyed by
// their BLAKE3 hash, so unchanged content is never uploaded twice. Small
// files are packed into chunks of about PackSize; pack boundaries depend
// on the files' own content hashes, so an inserted or removed file only
// changes the pack around it. Large files are split into PackSize pieces.
// Saving reuses the chunk ranges of the previous snapshot for files whose
// size and mtime did not change, so a second save reads only what changed.
//
// Restoring creates the tree with sparse placeholders of the right size
// and mtime (cargo compares mtimes) and fills file contents on first
// access (through the FUSE fill layer, see Mount) or in the background.
// The live cache is always the local directory.
package snapshot

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/blake3"

	"github.com/traumventures/cloudsourced/internal/blobcache"
)

// Store is where snapshots live (a scope of the pool's store).
type Store = blobcache.Store

// PackSize is the target size of a chunk.
const PackSize = 8 << 20

// smallFile is the largest file packed with others.
const smallFile = 1 << 20

// refreshAge is the age after which a chunk is uploaded again instead of
// reused, so that a snapshot stays restorable for at least
// 24h - refreshAge.
const refreshAge = 12 * time.Hour

// Version of the manifest format.
const Version = 1

// Ref is a range of a chunk.
type Ref struct {
	Hash string `json:"h"`
	Off  int64  `json:"o,omitempty"`
	Len  int64  `json:"l"`
}

// Entry is one file, directory or symlink, relative to the snapshot root.
type Entry struct {
	Path  string      `json:"p"`
	Mode  fs.FileMode `json:"m"`
	Size  int64       `json:"s,omitempty"`
	MTime int64       `json:"t"` // unix nanoseconds
	Link  string      `json:"k,omitempty"`
	Refs  []Ref       `json:"r,omitempty"`
}

// Manifest describes a snapshot.
type Manifest struct {
	Version int    `json:"version"`
	At      int64  `json:"at"` // unix seconds, also the manifest's name
	Bytes   int64  `json:"bytes"`
	Meta    []byte `json:"meta,omitempty"` // opaque data of the caller (slot metadata)
	// Entries are sorted by path; parents precede children.
	Entries []Entry `json:"entries"`
}

// Stats describes a save or restore.
type Stats struct {
	Bytes     int64
	Files     int64
	Chunks    int64 // chunks referenced
	NewChunks int64 // chunks uploaded (save) or fetched (restore)
	Seconds   float64
}

// ChunkKey is the store key of a chunk.
func ChunkKey(hash string) string { return "chunks/" + hash[:2] + "/" + hash }

func manifestKey(at int64) string { return "snapshots/" + strconv.FormatInt(at, 10) + ".json.gz" }

const latestKey = "snapshots/latest"

func hashOf(b []byte) string {
	s := blake3.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Options tune Save.
type Options struct {
	// Previous is the manifest of the last save of the same directory on
	// this machine (nil: read every file). Its chunks must still exist.
	Previous *Manifest
	// Parallel bounds concurrent uploads (0: 16).
	Parallel int
	// Meta is stored in the manifest.
	Meta []byte
	// Now is the snapshot time (zero: time.Now).
	Now time.Time
}

// Save snapshots dir into st and returns its manifest.
func Save(ctx context.Context, dir string, st Store, opt Options) (*Manifest, Stats, error) {
	begin := time.Now()
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	var entries []Entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: filepath.ToSlash(rel), Mode: fi.Mode(), MTime: fi.ModTime().UnixNano()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if e.Link, err = os.Readlink(p); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			e.Size = fi.Size()
		case fi.IsDir():
		default:
			return nil // sockets, devices: not cache content
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, Stats{}, err
	}
	prev := map[string]Entry{}
	// Chunks of the previous save are reused only while they are young:
	// stores expire objects after 24h, a snapshot must outlive its save.
	if opt.Previous != nil && now.Sub(time.Unix(opt.Previous.At, 0)) < refreshAge {
		for _, e := range opt.Previous.Entries {
			prev[e.Path] = e
		}
	}
	up := newUploader(ctx, st, opt.Parallel)
	var pack bytes.Buffer
	var packed []int // entry indexes in the current pack
	flush := func() {
		if pack.Len() == 0 {
			return
		}
		b := append([]byte(nil), pack.Bytes()...)
		h := hashOf(b)
		var off int64
		for _, i := range packed {
			entries[i].Refs = []Ref{{Hash: h, Off: off, Len: entries[i].Size}}
			off += entries[i].Size
		}
		up.put(h, b)
		pack.Reset()
		packed = packed[:0]
	}
	stats := Stats{}
	for i := range entries {
		e := &entries[i]
		if !e.Mode.IsRegular() {
			continue
		}
		stats.Files++
		stats.Bytes += e.Size
		if p, ok := prev[e.Path]; ok && p.Mode.IsRegular() && p.Size == e.Size && p.MTime == e.MTime && (len(p.Refs) > 0 || p.Size == 0) {
			e.Refs = p.Refs
			continue
		}
		if e.Size == 0 {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(e.Path))
		if e.Size <= smallFile {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, stats, err
			}
			if int64(len(b)) != e.Size {
				e.Size = int64(len(b)) // changed while walking: take what was read
			}
			pack.Write(b)
			packed = append(packed, i)
			// Content-defined boundary: stable under insertions elsewhere.
			if pack.Len() >= PackSize || (pack.Len() >= smallFile && blake3.Sum256(b)[0] < 16) {
				flush()
			}
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, stats, err
		}
		var refs []Ref
		buf := make([]byte, PackSize)
		var total int64
		for {
			n, rerr := io.ReadFull(f, buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				h := hashOf(b)
				refs = append(refs, Ref{Hash: h, Len: int64(n)})
				up.put(h, b)
				total += int64(n)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
					break
				}
				f.Close()
				return nil, stats, rerr
			}
		}
		f.Close()
		e.Refs, e.Size = refs, total
	}
	flush()
	if err := up.wait(); err != nil {
		return nil, stats, err
	}
	m := &Manifest{Version: Version, At: now.Unix(), Bytes: stats.Bytes, Meta: opt.Meta, Entries: entries}
	seen := map[string]bool{}
	for _, e := range entries {
		for _, r := range e.Refs {
			seen[r.Hash] = true
		}
	}
	stats.Chunks = int64(len(seen))
	stats.NewChunks = up.uploaded.Load()
	if err := writeManifest(ctx, st, m); err != nil {
		return nil, stats, err
	}
	stats.Seconds = time.Since(begin).Seconds()
	return m, stats, nil
}

func writeManifest(ctx context.Context, st Store, m *Manifest) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(m); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := st.Put(ctx, manifestKey(m.At), bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		return err
	}
	at := strconv.FormatInt(m.At, 10)
	return st.Put(ctx, latestKey, strings.NewReader(at), int64(len(at)))
}

// Latest returns the time of the newest snapshot in st.
func Latest(ctx context.Context, st Store) (int64, error) {
	rc, _, err := st.Get(ctx, latestKey)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 64))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Load reads the manifest of the snapshot taken at (unix seconds; 0: the
// newest).
func Load(ctx context.Context, st Store, at int64) (*Manifest, error) {
	if at == 0 {
		var err error
		if at, err = Latest(ctx, st); err != nil {
			return nil, fmt.Errorf("snapshot: latest: %w", err)
		}
	}
	rc, _, err := st.Get(ctx, manifestKey(at))
	if err != nil {
		return nil, fmt.Errorf("snapshot %d: %w", at, err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return nil, err
	}
	if m.Version != Version {
		return nil, fmt.Errorf("snapshot %d: unsupported version %d", at, m.Version)
	}
	return &m, nil
}

// uploader puts chunks concurrently, skipping those the store has.
type uploader struct {
	ctx      context.Context
	st       Store
	sem      chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	err      error
	done     map[string]bool
	uploaded atomic.Int64
}

func newUploader(ctx context.Context, st Store, parallel int) *uploader {
	if parallel <= 0 {
		parallel = 16
	}
	return &uploader{ctx: ctx, st: st, sem: make(chan struct{}, parallel), done: map[string]bool{}}
}

func (u *uploader) put(hash string, b []byte) {
	u.mu.Lock()
	if u.done[hash] || u.err != nil {
		u.mu.Unlock()
		return
	}
	u.done[hash] = true
	u.mu.Unlock()
	u.sem <- struct{}{}
	u.wg.Add(1)
	go func() {
		defer func() { <-u.sem; u.wg.Done() }()
		key := ChunkKey(hash)
		if info, err := u.st.Stat(u.ctx, key); err == nil && time.Since(info.ModTime) < refreshAge {
			return
		}
		if err := u.st.Put(u.ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
			u.mu.Lock()
			if u.err == nil {
				u.err = fmt.Errorf("upload chunk %s: %w", hash[:12], err)
			}
			u.mu.Unlock()
			return
		}
		u.uploaded.Add(1)
	}()
}

func (u *uploader) wait() error {
	u.wg.Wait()
	return u.err
}
