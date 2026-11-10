package telemetry

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// HeaderTraceParent is the W3C trace context header.
const HeaderTraceParent = "traceparent"

// Inject returns the W3C traceparent of the span in ctx ("" if none).
func Inject(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c[HeaderTraceParent]
}

// Extract returns ctx carrying the remote span context of traceparent
// (unchanged when traceparent is empty or malformed).
func Extract(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{HeaderTraceParent: traceparent})
}

// InjectHTTP adds the trace context of ctx to request headers.
func InjectHTTP(ctx context.Context, h http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

// ExtractHTTP returns ctx carrying the trace context of request headers.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(h))
}

// Args reduces a tool's arguments to what may be recorded: the subcommand
// (first non-flag argument, skipping "+toolchain") and the names of flags.
// Flag values and positional arguments are dropped, since they may contain
// secrets; short flags keep only their letter ("-j8" -> "-j").
func Args(args []string) (sub string, flags []string) {
	for _, a := range args {
		switch {
		case a == "--":
			return sub, flags
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a, "=")
			flags = append(flags, name)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			flags = append(flags, a[:2])
		case strings.HasPrefix(a, "+"):
		case sub == "":
			sub = a
		}
	}
	return sub, flags
}
