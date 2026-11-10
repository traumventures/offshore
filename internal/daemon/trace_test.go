package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/pool"
	"github.com/traumventures/cloudsourced/internal/tracing"
	"github.com/traumventures/cloudsourced/internal/tracing/tracingtest"
)

// eventPool is a pool that only serves events and remembers the trace
// context of the call.
type eventPool struct {
	pool.Pool
	got trace.SpanContext
}

func (f *eventPool) Events(ctx context.Context, since, until time.Time) ([]events.Event, error) {
	f.got = trace.SpanContextFromContext(ctx)
	return []events.Event{{Type: events.TaskEnd, Task: "t1"}}, nil
}

// TestRPCContinuesTrace: the proxy's traceparent in a daemon request
// becomes the parent of the daemon's work.
func TestRPCContinuesTrace(t *testing.T) {
	tracingtest.Record(t)
	f := &eventPool{}
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), pool: f, roots: map[string]*root{}, conns: map[string]*machineConn{}}
	d.metrics = newDaemonMetrics(d)
	ctx, span := tracing.Start(context.Background(), "cloudsourced.run")
	defer span.End()
	a, b := net.Pipe()
	go d.handle(context.Background(), b)
	json.NewEncoder(a).Encode(Request{Op: "events", TraceParent: tracing.Inject(ctx)})
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(a)).Decode(&resp); err != nil || !resp.Done || resp.Error != "" {
		t.Fatalf("response %+v %v", resp, err)
	}
	a.Close()
	var evs []events.Event
	json.Unmarshal(resp.Result, &evs)
	if len(evs) != 1 || evs[0].Task != "t1" {
		t.Fatalf("events %s", resp.Result)
	}
	if f.got.TraceID() != span.SpanContext().TraceID() || f.got.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("daemon did not continue the proxy's trace: %+v", f.got)
	}
}
