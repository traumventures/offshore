package worker

import (
	"context"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/tracing"
)

// startExecSpan starts the session span ("worker.exec"), continuing the
// trace of the tool run when the request carries one. Neither argv nor
// environment are recorded: they may contain secrets.
func (s *Worker) startExecSpan(ctx context.Context, req proto.Exec) (context.Context, trace.Span) {
	ctx = tracing.Extract(ctx, req.TraceParent)
	tool := ""
	if len(req.Argv) > 0 {
		tool = req.Argv[0]
	}
	sub := ""
	if len(req.Argv) > 1 {
		sub, _ = tracing.Args(req.Argv[1:])
	}
	return tracing.Start(ctx, "worker.exec", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("offshore.machine", s.cfg.MachineID),
		attribute.String("offshore.repo_id", req.RepoID),
		attribute.String("offshore.lease", req.Lease),
		attribute.String("offshore.tool", tool),
		attribute.String("offshore.subcommand", sub),
		attribute.Float64("offshore.reserve.cpu", req.Grant.Reserve.CPU),
		attribute.Int64("offshore.reserve.memory", req.Grant.Reserve.Memory),
		attribute.Int64("offshore.memory_max", req.Grant.MemoryMax),
	))
}

// exitAttrs records the session's cgroup usage, exit and cache counters.
func exitAttrs(span trace.Span, ex proto.Exit, req proto.Exec, clientGone bool) {
	u := ex.Usage
	span.SetAttributes(
		attribute.Int("process.exit.code", ex.Code),
		attribute.Int("offshore.signal", ex.Signal),
		attribute.Int64("offshore.usage.peak_memory", u.PeakMemory),
		attribute.Float64("offshore.usage.cpu_seconds", u.CPUSeconds),
		attribute.Float64("offshore.usage.wall_seconds", u.Wall.Seconds()),
		attribute.Bool("offshore.usage.oom", u.OOM),
		attribute.Bool("offshore.client_gone", clientGone),
	)
	if u.OOM {
		span.AddEvent("oom kill", trace.WithAttributes(attribute.Int64("offshore.memory_max", req.Grant.MemoryMax)))
	}
	keys := make([]string, 0, len(ex.Cache))
	for k := range ex.Cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var attrs []attribute.KeyValue
	for _, k := range keys {
		attrs = append(attrs, attribute.Int64("offshore.cache."+k, ex.Cache[k]))
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
		span.AddEvent("compiler cache", trace.WithAttributes(attrs...))
	}
}
