package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

// urlStore is a store behind pre-authenticated URL prefixes (OCI Object
// Storage pre-authenticated requests): an object key k is at base+k, read
// with GET/HEAD and written with PUT, without further credentials. The URL
// prefix is the grant: it only reaches objects below its scope and expires.
// Deleting is not possible through such URLs: Delete is a no-op and the
// bucket's lifecycle rule removes objects after a day; reads never serve
// objects older than the TTL.
type urlStore struct {
	base   string // ends in "/" (or is a prefix ending at a scope boundary)
	client *http.Client
	ttl    time.Duration
	now    func() time.Time
	spool  int64 // unknown or larger sizes are spooled to a temporary file
}

var _ Store = (*urlStore)(nil)

func newURLStore(base string, ttl time.Duration) *urlStore {
	return &urlStore{base: base, client: http.DefaultClient, ttl: blobcache.ClampTTL(ttl), now: time.Now, spool: 16 << 20}
}

func (s *urlStore) url(key string) (string, error) {
	if !blobcache.ValidKey(key) {
		return "", fmt.Errorf("blobcache: invalid key %q", key)
	}
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.base + strings.Join(parts, "/"), nil
}

func (s *urlStore) do(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Response, error) {
	u, err := s.url(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return s.client.Do(req)
}

// info validates a response: missing or expired objects are ErrNotFound.
func (s *urlStore) info(resp *http.Response) (blobcache.Info, error) {
	if resp.StatusCode == http.StatusNotFound {
		return blobcache.Info{}, blobcache.ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return blobcache.Info{}, fmt.Errorf("store: %s %s", resp.Request.Method, resp.Status)
	}
	mod, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil || s.now().Sub(mod) > s.ttl {
		return blobcache.Info{}, blobcache.ErrNotFound
	}
	return blobcache.Info{Size: max(resp.ContentLength, 0), ModTime: mod}, nil
}

func (s *urlStore) Get(ctx context.Context, key string) (io.ReadCloser, blobcache.Info, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, blobcache.Info{}, err
	}
	info, err := s.info(resp)
	if err != nil {
		resp.Body.Close()
		return nil, blobcache.Info{}, err
	}
	return resp.Body, info, nil
}

func (s *urlStore) Stat(ctx context.Context, key string) (blobcache.Info, error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil, 0)
	if err != nil {
		return blobcache.Info{}, err
	}
	resp.Body.Close()
	return s.info(resp)
}

// Put uploads with a known length (unknown sizes are spooled first).
func (s *urlStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	var body io.Reader
	var n int64
	if size >= 0 && size <= s.spool {
		b, err := io.ReadAll(io.LimitReader(r, size+1))
		if err != nil {
			return err
		}
		body, n = bytes.NewReader(b), int64(len(b))
	} else {
		f, err := os.CreateTemp("", "store-url-")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if n, err = io.Copy(f, r); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = f
	}
	if size >= 0 && n != size {
		return fmt.Errorf("blobcache: short body (%d of %d bytes)", n, size)
	}
	resp, err := s.do(ctx, http.MethodPut, key, body, n)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("store: PUT %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Delete is not possible through pre-authenticated URLs; objects expire
// (TTL on read, lifecycle rule in the bucket).
func (s *urlStore) Delete(context.Context, string) error { return nil }

// urlBase returns the URL prefix of scope within a grant's URLs: the URL of
// the granted scope it lies below, extended by the rest of the path.
func urlBase(g Grant, scope string) (string, bool) {
	scope = cleanScope(scope)
	for granted, base := range g.URLs {
		if scope == granted || strings.HasPrefix(scope, granted+"/") {
			rest := strings.TrimPrefix(strings.TrimPrefix(scope, granted), "/")
			if !strings.HasSuffix(base, "/") {
				base += "/"
			}
			if rest != "" {
				base += rest + "/"
			}
			return base, true
		}
	}
	return "", false
}
