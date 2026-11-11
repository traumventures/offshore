package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/metrics"
	"github.com/traumventures/offshore/internal/provider"
)

// pathMetrics serves the Prometheus text format (GET, API key).
const pathMetrics = "/metrics"

// MetricsSource is implemented by pools that expose metrics.
type MetricsSource interface {
	// WriteMetrics writes the pool's metrics in the text format.
	WriteMetrics(ctx context.Context, w io.Writer) error
}

var (
	_ MetricsSource = (*Local)(nil)
	_ MetricsSource = (*Remote)(nil)
)

// Registry is the metrics registry of the local pool (the orchestrator's).
func (l *Local) Registry() *metrics.Registry { return l.orch.Metrics() }

// Metrics returns the pool's metrics: orchestrator, pool state and the
// last probed metrics of every machine (re-probed when older than a
// second, so that finished sessions show up).
func (l *Local) Metrics(ctx context.Context) []metrics.Family {
	if err := l.orch.RefreshIfStale(ctx, time.Second); err != nil {
		l.log.Debug("metrics: refresh", "err", err)
	}
	return l.Registry().Gather()
}

// WriteMetrics implements MetricsSource.
func (l *Local) WriteMetrics(ctx context.Context, w io.Writer) error {
	return metrics.WriteText(w, l.Metrics(ctx))
}

func (l *Local) provName(p provider.Provider) string {
	for name, q := range l.provs {
		if q == p {
			return name
		}
	}
	return p.Name()
}

// imageBuilt records one environment image preparation.
func (l *Local) imageBuilt(name string, start time.Time, err error) {
	reg := l.Registry()
	result := "ok"
	if err != nil {
		result = "error"
		reg.Counter("offshore_provider_errors_total", "", "provider", "op").Inc(name, "ensure_image")
	}
	reg.Counter("offshore_image_builds_total", "Environment image preparations (build if missing, push) by provider and result.", "provider", "result").Inc(name, result)
	reg.Histogram("offshore_image_build_seconds", "Duration of environment image preparations.", metrics.DurationBuckets, "provider").Observe(time.Since(start).Seconds(), name)
}

// WriteMetrics fetches the control plane's metrics.
func (r *Remote) WriteMetrics(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+pathMetrics, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control plane metrics: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 64<<20))
	return err
}

// MetricsHandler serves /metrics without authentication, for a listener
// bound to a trusted address (--metrics-listen).
func (s *Server) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathMetrics {
			http.NotFound(w, r)
			return
		}
		s.serveMetrics(w, r)
	})
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	s.pool.WriteMetrics(r.Context(), w)
}

// knownPath bounds the path label of HTTP metrics.
func knownPath(p string) string {
	switch p {
	case pathEnv, pathAcquire, pathRelease, pathUnreachable, pathEstimate, pathStatus, pathReconcile, pathDown, pathHealth, pathMetrics, pathEvents, pathQueue:
		return p
	}
	return "other"
}

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID returns the client's X-Request-Id when it is well formed, or a
// new one.
func requestID(r *http.Request) string {
	if id := r.Header.Get(headerRequestID); requestIDRe.MatchString(id) {
		return id
	}
	return newRequestID()
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const headerRequestID = "X-Request-Id"

// statusRecorder captures the response code.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
