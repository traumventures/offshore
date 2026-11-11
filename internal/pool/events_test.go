package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/tracing"
	"github.com/traumventures/offshore/internal/tracing/tracingtest"
)

// TestControlPlaneTracePropagation: daemon → control plane calls carry the
// W3C traceparent header; the control plane's span is a child of the
// client span, and the release reaches the event log; /v1/events returns it.
func TestControlPlaneTracePropagation(t *testing.T) {
	rec := tracingtest.Record(t)
	u := &config.PoolConfig{Pool: config.Pool{ID: "t", MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s",
		Scheduler: config.Scheduler{Estimator: "static", CPUOvercommit: 1, MemoryOvercommit: 1}},
		Providers: map[string]map[string]any{"img": {"type": "test-img"}}, Telemetry: config.Telemetry{EventsRetentionDays: 7}}
	state := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
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

	ctx, root := tracing.Start(context.Background(), "daemon.release")
	if err := r.Release(ctx, "lease1", "repo/cargo/build", &scheduler.Usage{PeakMemory: 5, Wall: time.Second}); err != nil {
		t.Fatal(err)
	}
	root.End()
	tid := root.SpanContext().TraceID()
	var client, server sdktrace.ReadOnlySpan
	for i := 0; i < 100 && tracingtest.Find(rec, "controlplane /v1/release") != nil && len(rec.Ended()) < 3; i++ {
		time.Sleep(10 * time.Millisecond) // the server ends its span after the response
	}
	for _, s := range rec.Ended() {
		if s.Name() != "controlplane /v1/release" {
			continue
		}
		switch s.SpanKind() {
		case trace.SpanKindClient:
			client = s
		case trace.SpanKindServer:
			server = s
		}
	}
	if client == nil || server == nil || client.Parent().SpanID() != root.SpanContext().SpanID() ||
		server.Parent().SpanID() != client.SpanContext().SpanID() || !server.Parent().IsRemote() {
		t.Fatalf("want daemon.release > client > server:\n%s", tracingtest.Tree(rec, tid))
	}
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() != tid {
			t.Fatalf("span %s outside the trace", s.Name())
		}
	}
	evs, err := r.Events(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != events.TaskEnd || evs[0].Task != "lease1" || evs[0].Usage.PeakMemory != 5 || evs[0].TraceID != tid.String() {
		t.Fatalf("events via the control plane: %+v", evs)
	}
	bad, _ := NewRemote(ts.URL, hex.EncodeToString(sum[:]), "wrong-key-0123456789")
	if _, err := bad.Events(ctx, time.Time{}, time.Time{}); err == nil {
		t.Fatal("events without the API key")
	}
}
