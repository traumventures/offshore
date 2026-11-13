package blobcache

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newDir(t *testing.T, ttl time.Duration) (*DirStore, *clock) {
	t.Helper()
	d, err := NewDirStore(t.TempDir(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now()}
	d.now = c.now
	return d, c
}

func TestValidKey(t *testing.T) {
	good := []string{"a", "ns/.sccache_check", "ns/2/4/6/abc-def_1.x"}
	bad := []string{"", "/a", "a/", "a//b", "..", "a/../b", "./a", "a/.", "a b", "a\\b", "ä", "a/~put-1", strings.Repeat("x", 300)}
	for _, k := range good {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range bad {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
}

func TestClampTTL(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{0: DefaultTTL, -1: DefaultTTL, time.Hour: time.Hour, 48 * time.Hour: 48 * time.Hour, 30 * 24 * time.Hour: MaxTTL} {
		if got := ClampTTL(in); got != want {
			t.Errorf("ClampTTL(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestDirStoreTTL(t *testing.T) {
	ctx := context.Background()
	d, c := newDir(t, time.Hour)
	if err := d.Put(ctx, "ns/a/b", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	rc, info, err := d.Get(ctx, "ns/a/b")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" || info.Size != 5 {
		t.Fatalf("got %q size %d", b, info.Size)
	}
	c.t = c.t.Add(59 * time.Minute)
	if _, err := d.Stat(ctx, "ns/a/b"); err != nil {
		t.Fatalf("fresh entry: %v", err)
	}
	c.t = c.t.Add(2 * time.Minute)
	if _, err := d.Stat(ctx, "ns/a/b"); err != ErrNotFound {
		t.Fatalf("expired Stat: %v", err)
	}
	if _, _, err := d.Get(ctx, "ns/a/b"); err != ErrNotFound {
		t.Fatalf("expired Get: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "ns/a/b")); !os.IsNotExist(err) {
		t.Fatal("expired entry not removed on read")
	}
}

func TestDirStoreTTLIsCapped(t *testing.T) {
	d, _ := newDir(t, 30*24*time.Hour)
	if d.TTL() != MaxTTL {
		t.Fatalf("TTL %v not capped", d.TTL())
	}
}

func TestDirStoreSweep(t *testing.T) {
	ctx := context.Background()
	d, c := newDir(t, time.Hour)
	d.Put(ctx, "ns/old/x", strings.NewReader("1"), 1)
	c.t = c.t.Add(30 * time.Minute)
	d.Put(ctx, "ns/new/y", strings.NewReader("2"), 1)
	// A stale partial upload and a fresh one.
	stale := filepath.Join(d.dir, "ns", tmpPrefix+"stale")
	os.WriteFile(stale, []byte("x"), 0o644)
	os.Chtimes(stale, c.t.Add(-2*time.Hour), c.t.Add(-2*time.Hour))
	c.t = c.t.Add(45 * time.Minute)
	if n := d.Sweep(); n != 2 {
		t.Fatalf("Sweep removed %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "ns/old")); !os.IsNotExist(err) {
		t.Fatal("empty directory not removed")
	}
	if _, err := d.Stat(ctx, "ns/new/y"); err != nil {
		t.Fatalf("fresh entry swept: %v", err)
	}
}

func TestDirStoreShortBody(t *testing.T) {
	d, _ := newDir(t, time.Hour)
	if err := d.Put(context.Background(), "ns/k", strings.NewReader("abc"), 10); err == nil {
		t.Fatal("short body accepted")
	}
	if _, err := d.Stat(context.Background(), "ns/k"); err != ErrNotFound {
		t.Fatal("partial object visible")
	}
	entries, _ := os.ReadDir(filepath.Join(d.dir, "ns"))
	if len(entries) != 0 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func do(t *testing.T, h http.Handler, method, path string, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHandlerVerbs(t *testing.T) {
	d, c := newDir(t, time.Hour)
	h := &Handler{Store: d, Namespace: "env1", now: c.now}
	if w := do(t, h, "GET", "/k1", ""); w.Code != 404 {
		t.Fatalf("GET missing: %d", w.Code)
	}
	if w := do(t, h, "PROPFIND", "/a/b/k1", "", "Depth", "0"); w.Code != 404 {
		t.Fatalf("PROPFIND missing file: %d", w.Code)
	}
	if w := do(t, h, "MKCOL", "/a/", ""); w.Code != 201 {
		t.Fatalf("MKCOL: %d", w.Code)
	}
	if w := do(t, h, "PUT", "//a/b/k1", "0123456789"); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if _, err := d.Stat(context.Background(), "env1/a/b/k1"); err != nil {
		t.Fatalf("object not stored under the namespace: %v", err)
	}
	if w := do(t, h, "GET", "/a/b/k1", ""); w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatalf("GET: %d %q", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/a/b/k1", "", "Range", "bytes=2-4"); w.Code != 206 || w.Body.String() != "234" {
		t.Fatalf("GET range: %d %q", w.Code, w.Body)
	}
	if w := do(t, h, "HEAD", "/a/b/k1", ""); w.Code != 200 || w.Header().Get("Content-Length") != "10" || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %v", w.Code, w.Header())
	}
	w := do(t, h, "PROPFIND", "/a/b/k1", "", "Depth", "0")
	if w.Code != 207 {
		t.Fatalf("PROPFIND file: %d", w.Code)
	}
	var ms struct {
		Responses []struct {
			Href string `xml:"href"`
			Prop struct {
				Length       string `xml:"getcontentlength"`
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"propstat>prop"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &ms); err != nil || len(ms.Responses) != 1 {
		t.Fatalf("PROPFIND body %q: %v", w.Body, err)
	}
	if r := ms.Responses[0]; r.Prop.Length != "10" || r.Prop.ResourceType.Collection != nil || r.Href != "/a/b/k1" {
		t.Fatalf("PROPFIND file props: %+v", r)
	}
	w = do(t, h, "PROPFIND", "/a/b/", "", "Depth", "1")
	ms.Responses = nil
	if err := xml.Unmarshal(w.Body.Bytes(), &ms); err != nil || w.Code != 207 || ms.Responses[0].Prop.ResourceType.Collection == nil {
		t.Fatalf("PROPFIND collection: %d %q", w.Code, w.Body)
	}
	if w := do(t, h, "OPTIONS", "/", ""); w.Code != 200 || w.Header().Get("DAV") == "" {
		t.Fatalf("OPTIONS: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/a/", "x"); w.Code != 405 {
		t.Fatalf("PUT collection: %d", w.Code)
	}
	if w := do(t, h, "MOVE", "/a/b/k1", ""); w.Code != 405 {
		t.Fatalf("MOVE: %d", w.Code)
	}
	// Repository TTL below the store TTL.
	c.t = c.t.Add(20 * time.Minute)
	if w := do(t, h, "GET", "/_ttl/600/a/b/k1", ""); w.Code != 404 {
		t.Fatalf("GET older than _ttl: %d", w.Code)
	}
	if w := do(t, h, "PROPFIND", "/_ttl/600/a/b/k1", ""); w.Code != 404 {
		t.Fatalf("PROPFIND older than _ttl: %d", w.Code)
	}
	if w := do(t, h, "GET", "/_ttl/3600/a/b/k1", ""); w.Code != 200 {
		t.Fatalf("GET within _ttl: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/_ttl/600/a/b/k2", "new"); w.Code != 201 {
		t.Fatalf("PUT with _ttl: %d", w.Code)
	}
	if _, err := d.Stat(context.Background(), "env1/a/b/k2"); err != nil {
		t.Fatalf("_ttl segment became part of the key: %v", err)
	}
	if w := do(t, h, "DELETE", "/a/b/k1", ""); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if w := do(t, h, "GET", "/a/b/k1", ""); w.Code != 404 {
		t.Fatalf("GET deleted: %d", w.Code)
	}
	// The store TTL applies regardless of the URL.
	c.t = c.t.Add(2 * time.Hour)
	if w := do(t, h, "GET", "/_ttl/86400/a/b/k2", ""); w.Code != 404 {
		t.Fatalf("GET beyond store TTL: %d", w.Code)
	}
}

func TestHandlerPathSafety(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDirStore(filepath.Join(dir, "store"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: d, Namespace: "env1"}
	for _, p := range []string{"/../escape", "/a/../../escape", "/%2e%2e/escape", "/a/./b", "/a%20b", "/_ttl/x/k", "/_ttl/-5/k", "/~put-x"} {
		w := do(t, h, "PUT", p, "x")
		if w.Code != 400 {
			t.Errorf("PUT %s: %d, want 400", p, w.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Fatal("write escaped the store directory")
	}
	// Without a fixed namespace the first segment is the namespace.
	hm := &Handler{Store: d}
	if w := do(t, hm, "PUT", "/env2/k", "x"); w.Code != 201 {
		t.Fatalf("PUT namespaced: %d", w.Code)
	}
	if _, err := d.Stat(context.Background(), "env2/k"); err != nil {
		t.Fatal(err)
	}
	if w := do(t, hm, "PUT", "/env2", "x"); w.Code != 405 {
		t.Fatalf("PUT namespace root: %d", w.Code)
	}
	if w := do(t, hm, "GET", "/..", ""); w.Code != 400 {
		t.Fatalf("GET bad namespace: %d", w.Code)
	}
	// A fixed namespace cannot be escaped through the URL.
	if w := do(t, h, "GET", "/env2/k", ""); w.Code != 404 {
		t.Fatalf("fixed namespace leaked into another one: %d", w.Code)
	}
}

func TestHandlerMaxObject(t *testing.T) {
	d, _ := newDir(t, time.Hour)
	h := &Handler{Store: d, Namespace: "n", MaxObject: 4}
	if w := do(t, h, "PUT", "/k", "12345"); w.Code != 413 {
		t.Fatalf("oversized PUT: %d", w.Code)
	}
	req := httptest.NewRequest("PUT", "/k", io.NopCloser(strings.NewReader("123456")))
	req.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("oversized chunked PUT: %d", w.Code)
	}
	if _, err := d.Stat(context.Background(), "n/k"); err != ErrNotFound {
		t.Fatal("oversized object stored")
	}
}

// fakeS3 records requests and keeps objects in memory.
type fakeS3 struct {
	objects map[string][]byte
	mod     map[string]time.Time
	keys    []string
	lengths []int64
}

type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func (f *fakeS3) check(bucket *string, key *string) {
	if aws.ToString(bucket) != "bkt" {
		panic("wrong bucket " + aws.ToString(bucket))
	}
	f.keys = append(f.keys, aws.ToString(key))
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.check(in.Bucket, in.Key)
	b, ok := f.objects[*in.Key]
	if !ok {
		return nil, apiErr{"NoSuchKey"}
	}
	m := f.mod[*in.Key]
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), ContentLength: aws.Int64(int64(len(b))), LastModified: &m}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.check(in.Bucket, in.Key)
	b, ok := f.objects[*in.Key]
	if !ok {
		return nil, apiErr{"NotFound"}
	}
	m := f.mod[*in.Key]
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(b))), LastModified: &m}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.check(in.Bucket, in.Key)
	if _, ok := in.Body.(io.Seeker); !ok {
		panic("unseekable body")
	}
	b, _ := io.ReadAll(in.Body)
	f.objects[*in.Key] = b
	f.mod[*in.Key] = time.Now()
	f.lengths = append(f.lengths, aws.ToInt64(in.ContentLength))
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.check(in.Bucket, in.Key)
	delete(f.objects, *in.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3Store(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string][]byte{}, mod: map[string]time.Time{}}
	s := NewS3Store(f, "bkt", "/cache/sccache/", 30*24*time.Hour)
	if s.ttl != MaxTTL {
		t.Fatalf("ttl not capped: %v", s.ttl)
	}
	s = NewS3Store(f, "bkt", "/cache/sccache/", 0)
	if s.ttl != DefaultTTL {
		t.Fatalf("default ttl %v", s.ttl)
	}
	s.spool = 4
	if err := s.Put(ctx, "env/k1", strings.NewReader("ab"), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "env/k2", strings.NewReader("large body"), -1); err != nil { // spooled to a file
		t.Fatal(err)
	}
	if got := f.keys; len(got) != 2 || got[0] != "cache/sccache/env/k1" || got[1] != "cache/sccache/env/k2" {
		t.Fatalf("object keys %v", got)
	}
	if f.lengths[0] != 2 || f.lengths[1] != 10 {
		t.Fatalf("content lengths %v", f.lengths)
	}
	rc, info, err := s.Get(ctx, "env/k2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "large body" || info.Size != 10 {
		t.Fatalf("Get: %q %d", b, info.Size)
	}
	if _, err := s.Stat(ctx, "env/missing"); err != ErrNotFound {
		t.Fatalf("Stat missing: %v", err)
	}
	if _, _, err := s.Get(ctx, "env/missing"); err != ErrNotFound {
		t.Fatalf("Get missing: %v", err)
	}
	f.mod["cache/sccache/env/k1"] = time.Now().Add(-25 * time.Hour)
	if _, _, err := s.Get(ctx, "env/k1"); err != ErrNotFound {
		t.Fatalf("expired object served: %v", err)
	}
	if _, err := s.Stat(ctx, "env/k1"); err != ErrNotFound {
		t.Fatalf("expired object stat: %v", err)
	}
	if err := s.Put(ctx, "env/k3", strings.NewReader("abc"), 5); err == nil {
		t.Fatal("short body accepted")
	}
	if err := s.Put(ctx, "../x", strings.NewReader("a"), 1); err == nil {
		t.Fatal("invalid key accepted")
	}
	if err := s.Delete(ctx, "env/k2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["cache/sccache/env/k2"]; ok {
		t.Fatal("not deleted")
	}
	// Through the WebDAV handler in namespace-from-path mode.
	h := &Handler{Store: s}
	if w := do(t, h, "PUT", "/env/x", "hi"); w.Code != 201 {
		t.Fatalf("PUT via handler: %d", w.Code)
	}
	if w := do(t, h, "GET", "/env/x", ""); w.Code != 200 || w.Body.String() != "hi" || w.Header().Get("Content-Length") != "2" {
		t.Fatalf("GET via handler: %d %q", w.Code, w.Body)
	}
}

func TestParseS3Location(t *testing.T) {
	b, p, err := ParseS3Location("s3://my-bucket/cache/sccache/")
	if err != nil || b != "my-bucket" || p != "cache/sccache" {
		t.Fatalf("got %q %q %v", b, p, err)
	}
	if b, p, err = ParseS3Location("bkt"); err != nil || b != "bkt" || p != "" {
		t.Fatalf("bucket only: %q %q %v", b, p, err)
	}
	if _, _, err := ParseS3Location("/x"); err == nil {
		t.Fatal("empty bucket accepted")
	}
}
