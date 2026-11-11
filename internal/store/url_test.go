package store

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

// parServer imitates Object Storage pre-authenticated requests: objects
// below /p/<token>/ by name, GET/HEAD/PUT only.
func parServer(t *testing.T, token string) (*httptest.Server, map[string][]byte) {
	var mu sync.Mutex
	objs := map[string][]byte{}
	mod := map[string]time.Time{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		name, ok := strings.CutPrefix(r.URL.Path, "/p/"+token+"/o/")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if r.ContentLength < 0 {
				http.Error(w, "length required", http.StatusLengthRequired)
				return
			}
			b, _ := io.ReadAll(r.Body)
			objs[name], mod[name] = b, time.Now()
		case http.MethodGet, http.MethodHead:
			b, ok := objs[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Last-Modified", mod[name].UTC().Format(http.TimeFormat))
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			if r.Method == http.MethodGet {
				w.Write(b)
			}
		default:
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, objs
}

func TestURLStore(t *testing.T) {
	srv, objs := parServer(t, "tok")
	g := Grant{Store: "oci", Scopes: []string{"repos/r1"}, URLs: map[string]string{"repos/r1": srv.URL + "/p/tok/o/cache/pool/repos/r1/"}}
	ctx := context.Background()
	st, err := Open(g, "repos/r1/chunks", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Stat(ctx, "ab/cd"); !errors.Is(err, blobcache.ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if err := st.Put(ctx, "ab/cd", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "ab/unknown", strings.NewReader("spooled"), -1); err != nil {
		t.Fatal(err)
	}
	if _, ok := objs["cache/pool/repos/r1/chunks/ab/cd"]; !ok {
		t.Fatalf("object names: %v", objs)
	}
	rc, info, err := st.Get(ctx, "ab/cd")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" || info.Size != 5 {
		t.Fatalf("get %q %+v", b, info)
	}
	if err := st.Delete(ctx, "ab/cd"); err != nil {
		t.Fatalf("delete is a no-op: %v", err)
	}
	// Objects older than the TTL are not served.
	st2, _ := Open(g, "repos/r1", time.Hour)
	st2.(*urlStore).now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := st2.Stat(ctx, "chunks/ab/cd"); !errors.Is(err, blobcache.ErrNotFound) {
		t.Fatalf("expired object served: %v", err)
	}
	// Scopes outside the grant have no URL.
	if _, err := Open(g, "repos/r2", 0); !errors.Is(err, ErrScope) {
		t.Fatalf("foreign scope: %v", err)
	}
}

// A fresh pre-authenticated request is unknown to some frontends for a few
// seconds (404 BucketNotFound): requests are retried.
func TestURLStorePropagation(t *testing.T) {
	var mu sync.Mutex
	fails := 2
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":"BucketNotFound","message":"Either the bucket does not exist or you are not authorized"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	st := newURLStore(srv.URL+"/p/t/o/x/", 0)
	if err := st.Put(context.Background(), "k", strings.NewReader("data"), 4); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || bodies[2] != "data" {
		t.Fatalf("attempts %q", bodies)
	}
}
