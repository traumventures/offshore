// Package telemetrytest records spans in memory for tests.
package telemetrytest

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Record installs a global tracer provider that records every span until
// the test ends.
func Record(t testing.TB) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

// Find returns the first ended span named name (nil if none).
func Find(rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// Tree renders ended spans of trace id as "parent > child" names, for
// failure messages.
func Tree(rec *tracetest.SpanRecorder, id trace.TraceID) string {
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() == id {
			byID[s.SpanContext().SpanID()] = s
		}
	}
	var lines []string
	for _, s := range byID {
		path := s.Name()
		for p := s.Parent(); p.IsValid(); {
			ps, ok := byID[p.SpanID()]
			if !ok {
				path = "(remote) > " + path
				break
			}
			path = ps.Name() + " > " + path
			p = ps.Parent()
		}
		lines = append(lines, path)
	}
	return strings.Join(lines, "\n")
}
