package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Entry is one node of a layer tree: path -> type, mode, size, mtime, digest.
// This is the manifest shape a future internal/anyfs core would share
// between the source projection, outputfs and worker tiers.
type Entry struct {
	Path    string `json:"p"`           // slash separated, "" = root
	Type    byte   `json:"t"`           // 'd', 'f', 'l'
	Mode    uint32 `json:"m"`           // permission bits
	Size    int64  `json:"s,omitempty"` // regular files
	MtimeNs int64  `json:"mt"`
	Digest  string `json:"h,omitempty"` // sha256 of content (files)
	Target  string `json:"l,omitempty"` // symlink target
}

type Manifest struct {
	Entries []Entry `json:"entries"`
}

func loadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return &m, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// pack walks src, stores every regular file once by digest in store/ (hardlink
// with link=true, else copy) and writes the manifest.
func pack(src, store, manifestPath string, link bool, workers int) error {
	start := time.Now()
	if err := os.MkdirAll(store, 0o755); err != nil {
		return err
	}
	var entries []Entry
	type job struct {
		idx  int
		path string
		key  [2]uint64
	}
	var jobs []job
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			rel = ""
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			return err
		}
		e := Entry{Path: filepath.ToSlash(rel), Mode: st.Mode & 0o7777, MtimeNs: st.Mtim.Nano()}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			e.Type = 'd'
		case syscall.S_IFLNK:
			e.Type = 'l'
			e.Target, err = os.Readlink(p)
			if err != nil {
				return err
			}
		case syscall.S_IFREG:
			e.Type = 'f'
			e.Size = st.Size
			jobs = append(jobs, job{len(entries), p, [2]uint64{st.Dev, st.Ino}})
		default:
			return nil
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return err
	}
	var mu sync.Mutex
	byIno := map[[2]uint64]string{}
	var bytes, blobs int64
	ch := make(chan job)
	errc := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				mu.Lock()
				h, ok := byIno[j.key]
				mu.Unlock()
				if !ok {
					var err error
					if h, err = hashFile(j.path); err != nil {
						errc <- err
						return
					}
					dst := filepath.Join(store, h)
					mu.Lock()
					byIno[j.key] = h
					_, statErr := os.Lstat(dst)
					mu.Unlock()
					if os.IsNotExist(statErr) {
						if link {
							err = os.Link(j.path, dst)
							if os.IsExist(err) {
								err = nil
							}
						} else {
							err = copyFile(j.path, dst)
						}
						if err != nil {
							errc <- err
							return
						}
						mu.Lock()
						blobs++
						bytes += entries[j.idx].Size
						mu.Unlock()
					}
				}
				entries[j.idx].Digest = h
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
	}
	b, err := json.Marshal(Manifest{Entries: entries})
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath+".tmp", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(manifestPath+".tmp", manifestPath); err != nil {
		return err
	}
	fmt.Printf("pack: entries=%d files=%d new_blobs=%d new_bytes=%d secs=%.2f\n",
		len(entries), len(jobs), blobs, bytes, time.Since(start).Seconds())
	return nil
}
