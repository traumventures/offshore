package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/telemetry"
	"github.com/traumventures/cloudsourced/internal/telemetry/telemetrytest"
	"github.com/traumventures/cloudsourced/internal/transport"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestExecContinuesTrace: the session span continues the client's trace,
// from the Hello of the stream or from the Exec request (both optional).
func TestExecContinuesTrace(t *testing.T) {
	rec := telemetrytest.Record(t)
	m, _ := auth.LoadMaster(t.TempDir())
	addr := freeUDPPort(t)
	fp := setEnv(t, m, "m1", time.Now().Add(time.Hour), addr)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	var conn *quic.Conn
	for conn == nil {
		if conn, err = transport.Dial(dctx, addr, fp); err != nil {
			if dctx.Err() != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	defer conn.CloseWithError(0, "")
	session := auth.NewToken(m.Signer(), auth.ScopeSession, "m1", "l1", time.Now().Add(time.Hour))

	run := func(helloCtx context.Context, traceparent string) {
		t.Helper()
		_, pc, err := transport.OpenStream(helloCtx, conn, session, proto.KindExec)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing is attached: the session fails in preparation, after its
		// span started.
		pc.Send(proto.TExec, proto.Exec{Argv: []string{"cargo", "build", "--token=secret"}, Lease: "l1", Repo: "/r", TraceParent: traceparent})
		var pe *proto.Error
		if err := pc.Expect(proto.TStarted, nil); !errors.As(err, &pe) || pe.Stage != "prepare" {
			t.Fatalf("want prepare error, got %v", err)
		}
	}
	find := func(tid trace.TraceID) sdktrace.ReadOnlySpan {
		for i := 0; i < 200; i++ {
			for _, s := range rec.Ended() {
				if s.Name() == "server.exec" && s.SpanContext().TraceID() == tid {
					return s
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	hctx, hello := telemetry.Start(dctx, "proxy.connect")
	run(hctx, "")
	hello.End()
	s := find(hello.SpanContext().TraceID())
	if s == nil || s.Parent().SpanID() != hello.SpanContext().SpanID() || s.SpanKind() != trace.SpanKindServer {
		t.Fatalf("server.exec must continue the Hello's trace: %+v", s)
	}
	for _, kv := range s.Attributes() {
		if v := kv.Value.Emit(); kv.Key == "cloudsourced.subcommand" && v != "build" || strings.Contains(v, "secret") {
			t.Fatalf("attribute %s=%s", kv.Key, v)
		}
	}

	ectx, root := telemetry.Start(context.Background(), "cloudsourced.run")
	run(dctx, telemetry.Inject(ectx)) // Hello without trace context
	root.End()
	s = find(root.SpanContext().TraceID())
	if s == nil || s.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("server.exec must continue the Exec's trace")
	}
	if s.Status().Code.String() != "Error" {
		t.Fatalf("failed session span status %v", s.Status())
	}
}
