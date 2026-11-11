package worker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/zeebo/blake3"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/platform"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/sandbox"
	"github.com/traumventures/offshore/internal/store"
)

// Machine caches with a digest (e.g. node_modules per lockfile).
//
// The content is immutable per digest: the hash of the digest files'
// paths and contents, the cache's name, install command and paths, and
// the platform (os/arch/libc) of the environment. The first session that
// needs a digest runs the install command remotely (in a sandbox with the
// session's environment, the cache paths bound to a fresh directory) and
// stores the result in the machine store <deps dir>/digest/<name>/<digest>.
// Every slot gets a copy linked from the store on the same disk (reflinks,
// writable; otherwise hard links, mounted read-only so sessions cannot
// change the store), refreshed when the digest changes. The paths are
// hidden from the source projection, so they never come from the laptop.

// digestMeta is stored next to each digest's content for garbage
// collection.
type digestMeta struct {
	Paths    []string `json:"paths"`
	TTL      int64    `json:"ttl"`       // seconds
	MaxBytes int64    `json:"max_bytes"` // per cache name
	Bytes    int64    `json:"bytes"`
}

var digestLocks sync.Map // store dir -> *sync.Mutex

func (s *Worker) digestStore() string { return filepath.Join(s.cfg.DepsDir, "digest") }

// cacheDigest computes the digest of a cache in a repository checkout.
func cacheDigest(repo string, c proto.Cache, plat string) (string, error) {
	h := blake3.New()
	fmt.Fprintf(h, "offshore-digest-v1\x00%s\x00%s\x00%s\x00%s\x00", plat, c.Name, c.Install, strings.Join(c.Paths, "\x00"))
	var files []string
	for _, pat := range c.Digest {
		m, err := doublestar.Glob(os.DirFS(repo), filepath.ToSlash(filepath.Clean(pat)), doublestar.WithFilesOnly())
		if err != nil {
			return "", fmt.Errorf("digest %q: %w", pat, err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no digest files match %v", c.Digest)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			return "", err
		}
		sum := blake3.Sum256(b)
		fmt.Fprintf(h, "%s\x00%x\x00", f, sum)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// digestBinds makes a digest cache available to a session: it generates
// the digest's content if the machine store lacks it and links it into
// the slot. base are the session's other binds (toolchain, other caches),
// which the install command sees too.
func (s *Worker) digestBinds(req proto.Exec, c proto.Cache, env []string, slot string, base []sandbox.Bind, out io.Writer) ([]sandbox.Bind, error) {
	digest, err := cacheDigest(req.Repo, c, platform.Local().CacheKey())
	if err != nil {
		return nil, err
	}
	store := filepath.Join(s.digestStore(), c.Name, digest)
	if err := s.ensureDigest(req, c, env, store, base, out); err != nil {
		return nil, err
	}
	now := time.Now()
	os.Chtimes(filepath.Join(store, "meta.json"), now, now) // last use, for gc
	// Link into the slot unless it already holds this digest.
	dst := filepath.Join(slot, "digest", c.Name)
	var have string
	if m, err := readMeta(slot); err == nil {
		have = m.Digests[c.Name]
	}
	ro := false
	if b, err := os.ReadFile(filepath.Join(dst, ".linked")); err == nil {
		ro = string(b) == "hardlink"
	}
	if have != digest {
		os.RemoveAll(dst)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return nil, err
		}
		ro = false
		for i := range c.Paths {
			src, to := filepath.Join(store, "content", fmt.Sprint(i)), filepath.Join(dst, fmt.Sprint(i))
			if exec.Command("cp", "-a", "--reflink=always", src, to).Run() != nil {
				os.RemoveAll(to)
				if b, err := exec.Command("cp", "-al", src, to).CombinedOutput(); err != nil {
					return nil, fmt.Errorf("link %s: %v: %s", c.Name, err, b)
				}
				ro = true
			}
		}
		mode := "reflink"
		if ro {
			mode = "hardlink"
		}
		os.WriteFile(filepath.Join(dst, ".linked"), []byte(mode), 0o644)
		s.slots.update(slot, func(m *slotMeta) {
			if m.Digests == nil {
				m.Digests = map[string]string{}
			}
			m.Digests[c.Name] = digest
		})
	}
	var binds []sandbox.Bind
	for i, p := range c.Paths {
		binds = append(binds, sandbox.Bind{Src: filepath.Join(dst, fmt.Sprint(i)), Dst: filepath.Join(req.Repo, filepath.Clean(p)), ReadOnly: ro})
	}
	return binds, nil
}

// ensureDigest generates a digest's content in the machine store.
func (s *Worker) ensureDigest(req proto.Exec, c proto.Cache, env []string, store_ string, base []sandbox.Bind, out io.Writer) error {
	l, _ := digestLocks.LoadOrStore(store_, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(filepath.Join(store_, "meta.json")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(store_), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(store_), ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	binds := append([]sandbox.Bind{}, base...)
	for i, p := range c.Paths {
		dir := filepath.Join(tmp, "content", fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		binds = append(binds, sandbox.Bind{Src: dir, Dst: filepath.Join(req.Repo, filepath.Clean(p))})
	}
	begin := time.Now()
	digestScope := ""
	var envStore store.Store
	if req.Store != nil && s.cfg.EnvHash != "" && req.Store.Allows(store.EnvScope(s.cfg.EnvHash)) {
		digestScope = path.Join("digests", c.Name, filepath.Base(store_))
		envStore, _ = store.Open(*req.Store, store.EnvScope(s.cfg.EnvHash), blobcache.MaxTTL)
	}
	restored := false
	if envStore != nil {
		if err := loadDigest(context.Background(), subStore{envStore, digestScope}, filepath.Join(tmp, "content")); err == nil {
			restored = true
			fmt.Fprintf(out, "offshore: cache %s: restored from the store\n", c.Name)
		} else {
			for i := range c.Paths { // start the install from empty paths
				d := filepath.Join(tmp, "content", fmt.Sprint(i))
				os.RemoveAll(d)
				os.MkdirAll(d, 0o755)
			}
		}
	}
	if !restored {
		fmt.Fprintf(out, "offshore: cache %s: installing (%s)\n", c.Name, c.Install)
		if err := s.runInstall(req, c.Install, env, binds, out); err != nil {
			return fmt.Errorf("install %q: %w", c.Install, err)
		}
	}
	ttl := c.TTLSeconds
	if ttl <= 0 {
		ttl = int64(7 * 24 * time.Hour / time.Second)
	}
	meta := digestMeta{Paths: c.Paths, TTL: ttl, MaxBytes: c.MaxBytes, Bytes: treeSize(filepath.Join(tmp, "content"))}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, store_); err != nil {
		return err
	}
	if envStore != nil && !restored {
		// Other machines restore it instead of installing.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := saveDigest(ctx, subStore{envStore, digestScope}, filepath.Join(store_, "content")); err != nil {
				s.log.Warn("machine cache to the store", "cache", c.Name, "err", err)
			}
		}()
	}
	fmt.Fprintf(out, "offshore: cache %s: installed in %s (%d MB)\n", c.Name, time.Since(begin).Round(100*time.Millisecond), meta.Bytes>>20)
	return nil
}

// runInstall runs an install command in a sandbox (variable for tests).
var runInstallCmd = func(self string, spec sandbox.Spec, out io.Writer) error {
	cmd, err := sandbox.Command(self, spec)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (s *Worker) runInstall(req proto.Exec, install string, env []string, binds []sandbox.Bind, out io.Writer) error {
	return runInstallCmd(s.self, sandbox.Spec{Argv: []string{"/bin/sh", "-ec", install}, Env: env, Cwd: req.Repo, Binds: binds}, out)
}

// gcDigests removes digests unused for their TTL and, per cache name, the
// least recently used ones beyond max_bytes (the newest always stays).
func (s *Worker) gcDigests(now time.Time) (removed int) {
	names, _ := os.ReadDir(s.digestStore())
	for _, n := range names {
		dir := filepath.Join(s.digestStore(), n.Name())
		des, _ := os.ReadDir(dir)
		type ent struct {
			path string
			used time.Time
			meta digestMeta
		}
		var ents []ent
		for _, de := range des {
			p := filepath.Join(dir, de.Name())
			if strings.HasPrefix(de.Name(), ".install-") {
				if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > 24*time.Hour {
					os.RemoveAll(p) // abandoned install
				}
				continue
			}
			fi, err := os.Stat(filepath.Join(p, "meta.json"))
			if err != nil {
				continue
			}
			var m digestMeta
			if b, err := os.ReadFile(filepath.Join(p, "meta.json")); err == nil {
				json.Unmarshal(b, &m)
			}
			ents = append(ents, ent{p, fi.ModTime(), m})
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].used.After(ents[j].used) })
		var total int64
		for i, e := range ents {
			total += e.meta.Bytes
			expired := e.meta.TTL > 0 && now.Sub(e.used) > time.Duration(e.meta.TTL)*time.Second
			over := i > 0 && e.meta.MaxBytes > 0 && total > e.meta.MaxBytes
			if expired || over {
				os.RemoveAll(e.path)
				removed++
			}
		}
	}
	return removed
}

// digestHead names the layer of a digest's content in its store prefix.
const digestHead = "content"

// saveDigest writes a digest's (immutable) content to the store as one
// base layer with its pages.
func saveDigest(ctx context.Context, st blobcache.Store, dir string) error {
	pages := anyfs.NewStore(st, anyfs.StoreMedium())
	tree, _, err := anyfs.Scan(dir, anyfs.ScanOptions{Sink: func(k anyfs.Key, b []byte) error {
		return pages.Put(ctx, k, b, anyfs.Dirty)
	}})
	if err != nil {
		return err
	}
	l := anyfs.Commit(nil, nil, tree, time.Now(), nil)
	if err := anyfs.SaveLayer(ctx, st, l); err != nil {
		return err
	}
	return anyfs.SetHead(ctx, st, digestHead, l.ID)
}

// loadDigest extracts a digest's content saved by saveDigest into dir.
func loadDigest(ctx context.Context, st blobcache.Store, dir string) error {
	id, err := anyfs.Head(ctx, st, digestHead)
	if err != nil {
		return err
	}
	ch, err := anyfs.LoadChain(ctx, st, id)
	if err != nil {
		return err
	}
	return anyfs.Extract(ctx, anyfs.NewStore(st, anyfs.StoreMedium()), ch.Tree(), dir)
}

// subStore is a store below a key prefix.
type subStore struct {
	st     store.Store
	prefix string
}

func (p subStore) key(k string) string { return p.prefix + "/" + k }

func (p subStore) Get(ctx context.Context, k string) (io.ReadCloser, blobcache.Info, error) {
	return p.st.Get(ctx, p.key(k))
}
func (p subStore) Stat(ctx context.Context, k string) (blobcache.Info, error) {
	return p.st.Stat(ctx, p.key(k))
}
func (p subStore) Put(ctx context.Context, k string, r io.Reader, n int64) error {
	return p.st.Put(ctx, p.key(k), r, n)
}
func (p subStore) Delete(ctx context.Context, k string) error { return p.st.Delete(ctx, p.key(k)) }
