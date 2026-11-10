package fleet

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/orchestrator"
)

// maxEnvUpload bounds an uploaded environment build context.
const maxEnvUpload = 4 << 30

var envHashRe = regexp.MustCompile(`^[0-9a-f]{8,128}$`)

// Server exposes a Local fleet as the shared control plane API. All
// requests except /healthz need "Authorization: Bearer <api key>".
type Server struct {
	fleet    *Local
	apiKey   string
	stateDir string
	log      *slog.Logger
	// Reloader applies changes of the fleet configuration file before
	// scheduling calls (nil: the configuration is fixed).
	Reloader *Reloader
}

// NewServer creates the control plane handler. Uploaded environment
// contexts persist in stateDir/envs and are registered again on start.
func NewServer(f *Local, apiKey, stateDir string, log *slog.Logger) (*Server, error) {
	if apiKey == "" {
		return nil, errors.New("control plane: empty API key")
	}
	s := &Server{fleet: f, apiKey: apiKey, stateDir: stateDir, log: log}
	dir := s.envDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		hash, ok := strings.CutSuffix(e.Name(), ".tar.gz")
		if !ok || !envHashRe.MatchString(hash) {
			continue
		}
		tag, err := os.ReadFile(filepath.Join(dir, hash+".tag"))
		if err != nil {
			continue
		}
		f.Register(fileEnv(hash, strings.TrimSpace(string(tag)), filepath.Join(dir, e.Name())))
	}
	return s, nil
}

func (s *Server) envDir() string { return filepath.Join(s.stateDir, "envs") }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == pathHealth {
		w.Write([]byte("ok\n"))
		return
	}
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var h func(ctx context.Context, r *http.Request, progress io.Writer) (any, error)
	switch r.URL.Path {
	case pathEnv:
		h = s.env
	case pathAcquire:
		h = s.acquire
	case pathRelease:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req releaseReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return nil, s.fleet.Release(ctx, req.Lease, req.UsageKey, req.Usage)
		}
	case pathUnreachable:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req unreachableReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return nil, s.fleet.Unreachable(ctx, req.Lease, req.Machine)
		}
	case pathEstimate:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req estimateReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return s.fleet.Estimate(ctx, req.Key, req.Fallback)
		}
	case pathStatus:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			return s.fleet.Status(ctx) // machines carry no secrets; only leases carry tokens
		}
	case pathReconcile:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			if s.Reloader != nil {
				// Explicit reload; on error the previous configuration stays.
				if err := s.Reloader.Reload(); err != nil {
					return nil, fmt.Errorf("fleet configuration not reloaded: %w", err)
				}
			}
			return nil, s.fleet.Reconcile(ctx)
		}
	case pathDown:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			return nil, s.fleet.DestroyAll(ctx)
		}
	default:
		http.NotFound(w, r)
		return
	}
	s.stream(w, r, h)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}

// stream runs h and writes its progress and result as frames.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, h func(context.Context, *http.Request, io.Writer) (any, error)) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxies: do not buffer
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Headers go out immediately; calls may wait long before any frame.
	// Uploads are still read afterwards, which HTTP/1.1 needs full duplex for.
	http.NewResponseController(w).EnableFullDuplex()
	w.WriteHeader(http.StatusOK)
	flush()
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	progress := frameWriter{mu: &mu, enc: enc, flush: flush}
	start := time.Now()
	result, err := h(r.Context(), r, progress)
	mu.Lock()
	defer mu.Unlock()
	f := Frame{Done: true}
	if err != nil {
		f.Error = err.Error()
		s.log.Warn("request failed", "path", r.URL.Path, "err", err, "duration", time.Since(start).Round(time.Millisecond))
	} else if result != nil {
		f.Result, _ = json.Marshal(result)
	}
	enc.Encode(f)
	flush()
}

func (s *Server) acquire(ctx context.Context, r *http.Request, progress io.Writer) (any, error) {
	var req orchestrator.Request
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if s.Reloader != nil {
		s.Reloader.Check()
	}
	return s.fleet.Acquire(ctx, req, progress)
}

// env ensures an environment image. Without a body it only succeeds when
// the control plane already has the build context; otherwise the result
// asks the client to upload it (body: PackEnv output).
func (s *Server) env(ctx context.Context, r *http.Request, progress io.Writer) (any, error) {
	hash, tag := r.URL.Query().Get("hash"), r.URL.Query().Get("tag")
	if !envHashRe.MatchString(hash) || tag == "" || strings.ContainsAny(tag, " \n\t") {
		return nil, fmt.Errorf("invalid environment %q %q", hash, tag)
	}
	file := filepath.Join(s.envDir(), hash+".tar.gz")
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		tmp, err := os.CreateTemp(s.envDir(), "upload-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		_, err = io.Copy(tmp, http.MaxBytesReader(nil, r.Body, maxEnvUpload))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("environment upload: %w", err)
		}
		// Validate before accepting it.
		probe := fileEnv(hash, tag, tmp.Name())
		spec, dir, err := probe.Context()
		if err != nil {
			return nil, fmt.Errorf("environment upload: %w", err)
		}
		os.RemoveAll(dir)
		if spec.Tag != tag {
			return nil, fmt.Errorf("environment upload: tag %q does not match %q", spec.Tag, tag)
		}
		if err := os.WriteFile(filepath.Join(s.envDir(), hash+".tag"), []byte(tag+"\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp.Name(), file); err != nil {
			return nil, err
		}
		s.fleet.Register(fileEnv(hash, tag, file))
	}
	if !s.fleet.Known(hash) {
		return envResult{NeedContext: true}, nil
	}
	s.fleet.mu.Lock()
	env := s.fleet.envs[hash]
	s.fleet.mu.Unlock()
	return envResult{}, s.fleet.EnsureEnv(ctx, env, progress)
}
