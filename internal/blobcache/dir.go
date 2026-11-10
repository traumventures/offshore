package blobcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirStore keeps objects as files below a directory. Several processes may
// share the directory (writes are atomic renames). Expired objects are
// never served; reads remove them inline and Sweep removes the rest.
type DirStore struct {
	dir string
	ttl time.Duration
	now func() time.Time
}

// tmpPrefix marks partial uploads; keys cannot start with it because "~"
// is not a valid key character.
const tmpPrefix = "~put-"

// NewDirStore creates the directory if needed. ttl is clamped to MaxTTL.
func NewDirStore(dir string, ttl time.Duration) (*DirStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DirStore{dir: dir, ttl: ClampTTL(ttl), now: time.Now}, nil
}

// TTL returns the store's time to live.
func (d *DirStore) TTL() time.Duration { return d.ttl }

func (d *DirStore) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("blobcache: invalid key %q", key)
	}
	return filepath.Join(d.dir, filepath.FromSlash(key)), nil
}

// Get implements Store.
func (d *DirStore) Get(_ context.Context, key string) (io.ReadCloser, Info, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, Info{}, ErrNotFound
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, Info{}, ErrNotFound
	}
	if expired(fi.ModTime(), d.now(), d.ttl) {
		f.Close()
		os.Remove(p)
		return nil, Info{}, ErrNotFound
	}
	return f, Info{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Stat implements Store.
func (d *DirStore) Stat(_ context.Context, key string) (Info, error) {
	p, err := d.path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || expired(fi.ModTime(), d.now(), d.ttl) {
		return Info{}, ErrNotFound
	}
	return Info{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Put implements Store.
func (d *DirStore) Put(_ context.Context, key string, r io.Reader, size int64) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), tmpPrefix+"*")
	if errors.Is(err, fs.ErrNotExist) {
		// A concurrent Sweep removed the (empty) directory.
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		f, err = os.CreateTemp(filepath.Dir(p), tmpPrefix+"*")
	}
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("blobcache: short body (%d of %d bytes)", n, size)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// The modification time is the write time: the TTL counts from it.
		now := d.now()
		os.Chtimes(f.Name(), now, now)
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Delete implements Store.
func (d *DirStore) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep removes expired objects, stale partial uploads and empty
// directories. It returns the number of removed objects.
func (d *DirStore) Sweep() int {
	now := d.now()
	n := 0
	var dirs []string
	filepath.WalkDir(d.dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() {
			if p != d.dir {
				dirs = append(dirs, p)
			}
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return nil
		}
		ttl := d.ttl
		if strings.HasPrefix(de.Name(), tmpPrefix) {
			ttl = time.Hour
		}
		if expired(fi.ModTime(), now, ttl) && os.Remove(p) == nil {
			n++
		}
		return nil
	})
	// Deepest first; non-empty directories fail to remove.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
	return n
}

// SweepEvery runs Sweep periodically until ctx ends.
func (d *DirStore) SweepEvery(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Sweep()
		}
	}
}
