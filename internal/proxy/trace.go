package proxy

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/daemon"
	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/logs"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/tracing"
)

// runTrace is the root span of one tool run ("cloudsourced.run"). Its
// decision attributes are taken from the run record when it ends.
type runTrace struct {
	ctx   context.Context
	span  trace.Span
	rec   *events.Run
	user  string
	ended bool
}

// active is the trace of this process, ended before exec replaces it.
var active *runTrace

// startTrace sets up tracing for the proxy (client configuration and
// OTEL_* environment) and starts the root span.
func startTrace(tool string, args []string) *runTrace {
	tr := &runTrace{ctx: context.Background()}
	var tc config.Telemetry
	dir := ""
	if c, err := config.LoadClient(); err == nil {
		tc, dir, tr.user = c.Telemetry, c.Dir(), c.User
	}
	tracing.SetupFromConfig(tracing.ServiceProxy, tc, dir, logs.FromEnv())
	sub, flags := tracing.Args(args)
	tr.ctx, tr.span = tracing.Start(tr.ctx, "cloudsourced.run", trace.WithAttributes(
		attribute.String("cloudsourced.tool", tool),
		attribute.String("cloudsourced.subcommand", sub),
		attribute.StringSlice("cloudsourced.flags", flags),
	))
	active = tr
	return tr
}

// finish ends the root span (once) and flushes the exporter. code < 0
// means the process continues as the local tool (exec).
func (tr *runTrace) finish(code int) {
	if tr == nil || tr.ended {
		return
	}
	tr.ended = true
	if r := tr.rec; r != nil {
		decision := r.Mode
		if r.Mode == events.Local && strings.HasPrefix(r.Why, "fallback:") {
			decision = "fallback"
		}
		tr.span.SetAttributes(
			attribute.String("cloudsourced.decision", decision),
			attribute.String("cloudsourced.reason", r.Why),
			attribute.String("cloudsourced.repo_id", daemon.RepoID(tr.user, r.Repo)),
		)
		if r.Machine != "" {
			tr.span.SetAttributes(attribute.String("cloudsourced.machine", r.Machine), attribute.String("cloudsourced.lease", r.Lease),
				attribute.String("cloudsourced.provider", r.Provider), attribute.String("cloudsourced.size", r.Size))
		}
	} else {
		tr.span.SetAttributes(attribute.String("cloudsourced.decision", events.Local), attribute.String("cloudsourced.reason", "not in a repository"))
	}
	if code >= 0 {
		tr.span.SetAttributes(attribute.Int("process.exit.code", code))
		if code != 0 {
			tr.span.SetStatus(codes.Error, "exit code")
		}
	}
	tr.span.End()
	tracing.Shutdown(3 * time.Second)
}

// execAttrs records the remote process outcome on a span.
func execAttrs(span trace.Span, ex proto.Exit) {
	u := ex.Usage
	span.SetAttributes(
		attribute.Int("process.exit.code", ex.Code),
		attribute.Int64("cloudsourced.usage.peak_memory", u.PeakMemory),
		attribute.Float64("cloudsourced.usage.cpu_seconds", u.CPUSeconds),
		attribute.Float64("cloudsourced.usage.wall_seconds", u.Wall.Seconds()),
		attribute.Bool("cloudsourced.usage.oom", u.OOM),
		attribute.Bool("cloudsourced.usage.warm", u.Warm),
	)
	keys := make([]string, 0, len(ex.Cache))
	for k := range ex.Cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		span.SetAttributes(attribute.Int64("cloudsourced.cache."+k, ex.Cache[k]))
	}
}
