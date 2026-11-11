package pool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/config"
)

func TestControlPlaneMetricsAndRequestIDs(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{ID: "t", MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s", MaxMachines: 1,
		Scheduler: config.Scheduler{Estimator: "static", CPUOvercommit: 1, MemoryOvercommit: 1}},
		Providers: map[string]map[string]any{"img": {"type": "test-img"}}}
	state := t.TempDir()
	var logbuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logbuf, nil))
	local, err := NewLocal(u, state, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(local, "secret-key-0123456789", state, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv)
	defer ts.Close()
	sum := sha256.Sum256(ts.Certificate().Raw)
	r, _ := NewRemote(ts.URL, hex.EncodeToString(sum[:]), "secret-key-0123456789")
	ctx := context.Background()
	if err := r.EnsureEnv(ctx, testEnv(t), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Status(ctx); err != nil {
		t.Fatal(err)
	}

	// Without the key /metrics is refused on the API port.
	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("unauthenticated /metrics: %d %q", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	// A client request id is kept.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+pathStatus, nil)
	req.Header.Set("Authorization", "Bearer secret-key-0123456789")
	req.Header.Set("X-Request-Id", "trace-42")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Request-Id") != "trace-42" {
		t.Fatalf("request id %q", resp.Header.Get("X-Request-Id"))
	}
	if !strings.Contains(logbuf.String(), `"request_id":"trace-42"`) {
		t.Fatalf("request id not logged: %s", logbuf.String())
	}

	var text bytes.Buffer
	if err := r.WriteMetrics(ctx, &text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`offshore_image_builds_total{provider="img",result="ok"} 1`,
		`offshore_http_requests_total{path="/v1/env",result="200"}`,
		`offshore_http_requests_total{path="/metrics",result="401"} 1`,
		`offshore_http_requests_total{path="/v1/status",result="200"} 2`,
		`offshore_cost_per_hour{provider="img"} 0`,
		"# TYPE offshore_machines gauge",
		"# TYPE offshore_http_request_duration_seconds histogram",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q in\n%s", want, text.String())
		}
	}

	// The unauthenticated listener serves only /metrics.
	plain := httptest.NewServer(srv.MetricsHandler())
	defer plain.Close()
	resp, err = http.Get(plain.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") || !strings.Contains(string(b), "offshore_queue_length 0") {
		t.Fatalf("plain metrics: %d %q\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	resp, _ = http.Post(plain.URL+pathStatus, "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plain listener served %s: %d", pathStatus, resp.StatusCode)
	}
}
