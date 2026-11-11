package proxy

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/logs"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/tracing"
)

// runTrace is the root span of one tool run ("offshore.run"). Its
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
	tr.ctx, tr.span = tracing.Start(tr.ctx, "offshore.run", trace.WithAttributes(
		attribute.String("offshore.tool", tool),
		attribute.String("offshore.subcommand", sub),
		attribute.StringSlice("offshore.flags", flags),
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
			attribute.String("offshore.decision", decision),
			attribute.String("offshore.reason", r.Why),
			attribute.String("offshore.repo_id", daemon.RepoID(tr.user, r.Repo)),
		)
		if r.Machine != "" {
			tr.span.SetAttributes(attribute.String("offshore.machine", r.Machine), attribute.String("offshore.lease", r.Lease),
				attribute.String("offshore.provider", r.Provider), attribute.String("offshore.size", r.Size))
		}
	} else {
		tr.span.SetAttributes(attribute.String("offshore.decision", events.Local), attribute.String("offshore.reason", "not in a repository"))
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
		attribute.Int64("offshore.usage.peak_memory", u.PeakMemory),
		attribute.Float64("offshore.usage.cpu_seconds", u.CPUSeconds),
		attribute.Float64("offshore.usage.wall_seconds", u.Wall.Seconds()),
		attribute.Bool("offshore.usage.oom", u.OOM),
		attribute.Bool("offshore.usage.warm", u.Warm),
	)
	keys := make([]string, 0, len(ex.Cache))
	for k := range ex.Cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		span.SetAttributes(attribute.Int64("offshore.cache."+k, ex.Cache[k]))
	}
}
