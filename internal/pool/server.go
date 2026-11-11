package pool

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
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/traumventures/offshore/internal/metrics"
	"github.com/traumventures/offshore/internal/orchestrator"
)

// maxEnvUpload bounds an uploaded environment build context.
const maxEnvUpload = 4 << 30

var envHashRe = regexp.MustCompile(`^[0-9a-f]{8,128}$`)

// Server exposes a Local pool as the shared control plane API. All
// requests except /healthz need "Authorization: Bearer <api key>".
type Server struct {
	pool     *Local
	apiKey   string
	stateDir string
	log      *slog.Logger
	// Reloader applies changes of the pool configuration file before
	// scheduling calls (nil: the configuration is fixed).
	Reloader *Reloader
}

// NewServer creates the control plane handler. Uploaded environment
// contexts persist in stateDir/envs and are registered again on start.
func NewServer(f *Local, apiKey, stateDir string, log *slog.Logger) (*Server, error) {
	if apiKey == "" {
		return nil, errors.New("control plane: empty API key")
	}
	s := &Server{pool: f, apiKey: apiKey, stateDir: stateDir, log: log}
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

// ServeHTTP tags every request with an id (X-Request-Id, taken from the
// client when well formed), logs it and counts it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == pathHealth {
		w.Write([]byte("ok\n"))
		return
	}
	start := time.Now()
	id := requestID(r)
	w.Header().Set(headerRequestID, id)
	rec := &statusRecorder{ResponseWriter: w}
	log := s.log.With("request_id", id)
	r, span := serverSpan(r, knownPath(r.URL.Path))
	failed := s.serve(rec, r, log)
	path, code := knownPath(r.URL.Path), rec.code
	if code == 0 {
		code = http.StatusOK
	}
	span.SetAttributes(attribute.Int("http.response.status_code", code), attribute.Bool("offshore.failed", failed))
	if failed || code >= 400 {
		span.SetStatus(codes.Error, strconv.Itoa(code))
	}
	span.End()
	result := strconv.Itoa(code)
	if failed {
		result = "error"
	}
	reg := s.pool.Registry()
	reg.Counter("offshore_http_requests_total", "Control plane API requests by path and result (HTTP status, or error for failed calls).", "path", "result").Inc(path, result)
	reg.Histogram("offshore_http_request_duration_seconds", "Control plane API request duration (acquire includes queueing).", metrics.DurationBuckets, "path").Observe(time.Since(start).Seconds(), path)
	if path != pathMetrics {
		log.Info("request", "method", r.Method, "path", r.URL.Path, "code", code, "failed", failed, "duration", time.Since(start).Round(time.Millisecond))
	}
}

// serve handles one request; it reports whether a streamed call failed.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, log *slog.Logger) bool {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if r.URL.Path == pathMetrics {
		s.serveMetrics(w, r)
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	var h func(ctx context.Context, r *http.Request, progress io.Writer) (any, error)
	switch r.URL.Path {
	case pathEnv:
		h = s.env
	case pathAcquire:
		h = func(ctx context.Context, r *http.Request, progress io.Writer) (any, error) {
			lease, err := s.acquire(ctx, r, progress)
			if err == nil {
				log.Info("lease", "lease", lease.ID, "machine", lease.MachineID, "provider", lease.Provider, "size", lease.Size, "reason", lease.Reason)
			}
			return lease, err
		}
	case pathRelease:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req releaseReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return nil, s.pool.Release(ctx, req.Lease, req.UsageKey, req.Usage)
		}
	case pathUnreachable:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req unreachableReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return nil, s.pool.Unreachable(ctx, req.Lease, req.Machine)
		}
	case pathEstimate:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req estimateReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return s.pool.Estimate(ctx, req.Key, req.Fallback)
		}
	case pathOutputsStore:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) {
			var req outputsStoreReq
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return s.pool.OutputsStore(ctx, req.Repo)
		}
	case pathStatus:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			return s.pool.Status(ctx) // machines carry no secrets; only leases carry tokens
		}
	case pathReconcile:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			if s.Reloader != nil {
				// Explicit reload; on error the previous configuration stays.
				if err := s.Reloader.Reload(); err != nil {
					return nil, fmt.Errorf("pool configuration not reloaded: %w", err)
				}
			}
			return nil, s.pool.Reconcile(ctx)
		}
	case pathDown:
		h = func(ctx context.Context, _ *http.Request, _ io.Writer) (any, error) {
			return nil, s.pool.DestroyAll(ctx)
		}
	case pathEvents:
		h = func(ctx context.Context, r *http.Request, _ io.Writer) (any, error) { return s.serveEvents(ctx, r) }
	case pathQueue:
		h = s.serveQueue
	default:
		http.NotFound(w, r)
		return false
	}
	return s.stream(w, r, h, log)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}

// stream runs h and writes its progress and result as frames.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, h func(context.Context, *http.Request, io.Writer) (any, error), log *slog.Logger) bool {
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
		log.Warn("request failed", "path", r.URL.Path, "err", err, "duration", time.Since(start).Round(time.Millisecond))
	} else if result != nil {
		f.Result, _ = json.Marshal(result)
	}
	enc.Encode(f)
	flush()
	return err != nil
}

func (s *Server) acquire(ctx context.Context, r *http.Request, progress io.Writer) (orchestrator.Lease, error) {
	var req orchestrator.Request
	if err := decode(r, &req); err != nil {
		return orchestrator.Lease{}, err
	}
	if s.Reloader != nil {
		s.Reloader.Check()
	}
	return s.pool.Acquire(ctx, req, progress)
}

// env ensures an environment image. Without a body it only succeeds when
// the control plane already has the build context; otherwise the result
// asks the client to upload it (body: PackEnv output).
func (s *Server) env(ctx context.Context, r *http.Request, progress io.Writer) (any, error) {
	hash, tag := r.URL.Query().Get("hash"), r.URL.Query().Get("tag")
	if !envHashRe.MatchString(hash) || tag == "" || strings.ContainsAny(tag, " \n\t") {
		return nil, fmt.Errorf("invalid environment %q %q", hash, tag)
	}
	if r.URL.Query().Get("state") == "1" {
		// Read-only (offshore doctor): no registration, no build.
		return s.pool.EnvState(ctx, hash, tag)
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
		s.pool.Register(fileEnv(hash, tag, file))
	}
	if !s.pool.Known(hash) {
		return envResult{NeedContext: true}, nil
	}
	s.pool.mu.Lock()
	env := s.pool.envs[hash]
	s.pool.mu.Unlock()
	if r.URL.Query().Get("prebuild") == "1" {
		s.prebuildEnv(env)
		return envResult{}, nil
	}
	return envResult{}, s.pool.EnsureEnv(ctx, env, progress)
}
