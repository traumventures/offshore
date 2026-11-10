package blobcache

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxObject bounds the size of one uploaded object.
const DefaultMaxObject = 2 << 30

// Handler serves a Store over the WebDAV subset used by sccache.
type Handler struct {
	Store Store
	// Namespace, if set, prefixes every key and is not taken from the URL.
	// If empty, the first path segment is the namespace.
	Namespace string
	MaxObject int64        // 0 means DefaultMaxObject
	Log       *slog.Logger // optional

	now func() time.Time // for tests
}

// target is a parsed request path.
type target struct {
	key    string        // full store key ("" for a collection)
	dir    bool          // the path names a collection
	maxAge time.Duration // 0 = store TTL only
	href   string        // request path for multistatus responses
}

var errBadPath = errors.New("invalid path")

func (h *Handler) parse(p string) (target, error) {
	t := target{href: p}
	// Clients join endpoint and key naively ("http://h/" + "/key"); empty
	// segments carry no meaning.
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	rest := strings.TrimPrefix(p, "/")
	t.dir = rest == "" || strings.HasSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, "/")
	var segs []string
	if rest != "" {
		segs = strings.Split(rest, "/")
	}
	ns := h.Namespace
	if ns == "" {
		if len(segs) == 0 {
			return t, nil // the root above all namespaces
		}
		ns, segs = segs[0], segs[1:]
		if len(segs) == 0 {
			t.dir = true
		}
	}
	if !validSegment(ns) {
		return t, errBadPath
	}
	if len(segs) > 0 && segs[0] == "_ttl" {
		if len(segs) < 2 {
			t.dir = true
			segs = nil
		} else {
			n, err := strconv.ParseInt(segs[1], 10, 64)
			if err != nil || n <= 0 {
				return t, errBadPath
			}
			t.maxAge = time.Duration(n) * time.Second
			segs = segs[2:]
			if len(segs) == 0 {
				t.dir = true
			}
		}
	}
	if len(segs) == 0 {
		return t, nil
	}
	key := strings.Join(append([]string{ns}, segs...), "/")
	if !ValidKey(key) {
		return t, errBadPath
	}
	t.key = key
	return t, nil
}

func (h *Handler) fresh(t target, info Info) bool {
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	return t.maxAge <= 0 || !expired(info.ModTime, now(), t.maxAge)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, err := h.parse(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.Log != nil {
		h.Log.Debug("blobcache", "method", r.Method, "path", r.URL.Path)
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, PROPFIND")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		if t.dir || t.key == "" {
			http.Error(w, "not a file", http.StatusNotFound)
			return
		}
		rc, info, err := h.Store.Get(ctx, t.key)
		if err != nil {
			h.storeError(w, err)
			return
		}
		defer rc.Close()
		if !h.fresh(t, info) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if rs, ok := rc.(io.ReadSeeker); ok {
			http.ServeContent(w, r, "", info.ModTime, rs)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			io.Copy(w, rc)
		}
	case http.MethodPut:
		if t.dir || t.key == "" {
			http.Error(w, "cannot write a collection", http.StatusMethodNotAllowed)
			return
		}
		limit := h.MaxObject
		if limit <= 0 {
			limit = DefaultMaxObject
		}
		if r.ContentLength > limit {
			http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
			return
		}
		body := http.MaxBytesReader(w, r.Body, limit)
		if err := h.Store.Put(ctx, t.key, body, r.ContentLength); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
				return
			}
			h.storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if t.key == "" {
			http.Error(w, "cannot delete a collection", http.StatusMethodNotAllowed)
			return
		}
		if err := h.Store.Delete(ctx, t.key); err != nil {
			h.storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "MKCOL":
		// Collections are virtual: they always exist.
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		h.propfind(w, r, t)
	default:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, PROPFIND")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if h.Log != nil {
		h.Log.Warn("blobcache store", "err", err)
	}
	http.Error(w, "store error", http.StatusInternalServerError)
}

type msResponse struct {
	Href     string     `xml:"D:href"`
	Propstat msPropstat `xml:"D:propstat"`
}

type msPropstat struct {
	Prop   msProp `xml:"D:prop"`
	Status string `xml:"D:status"`
}

type msResourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}

type msProp struct {
	ResourceType  msResourceType `xml:"D:resourcetype"`
	ContentLength *int64         `xml:"D:getcontentlength,omitempty"`
	LastModified  string         `xml:"D:getlastmodified,omitempty"`
	ContentType   string         `xml:"D:getcontenttype,omitempty"`
	ETag          string         `xml:"D:getetag,omitempty"`
}

type multistatus struct {
	XMLName   xml.Name     `xml:"D:multistatus"`
	XMLNS     string       `xml:"xmlns:D,attr"`
	Responses []msResponse `xml:"D:response"`
}

// propfind answers depth 0 and 1 the same way: collections are virtual and
// listed without members (sccache never lists).
func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, t target) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	resp := msResponse{Href: t.href, Propstat: msPropstat{Status: "HTTP/1.1 200 OK"}}
	if t.dir || t.key == "" {
		resp.Propstat.Prop.ResourceType.Collection = &struct{}{}
		resp.Propstat.Prop.LastModified = time.Now().UTC().Format(http.TimeFormat)
	} else {
		info, err := h.Store.Stat(r.Context(), t.key)
		if err == nil && !h.fresh(t, info) {
			err = ErrNotFound
		}
		if err != nil {
			h.storeError(w, err)
			return
		}
		size := info.Size
		resp.Propstat.Prop.ContentLength = &size
		resp.Propstat.Prop.LastModified = info.ModTime.UTC().Format(http.TimeFormat)
		resp.Propstat.Prop.ContentType = "application/octet-stream"
		resp.Propstat.Prop.ETag = fmt.Sprintf("\"%x-%x\"", info.ModTime.UnixNano(), info.Size)
	}
	b, err := xml.Marshal(multistatus{XMLNS: "DAV:", Responses: []msResponse{resp}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(xml.Header))
	w.Write(b)
}
