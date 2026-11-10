// Package telemetry sets up OpenTelemetry tracing for every cloudsourced
// component and propagates W3C trace context between them.
//
// Tracing is off by default. It is enabled by the standard environment
// variables (OTEL_TRACES_EXPORTER=otlp|console|none,
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, OTEL_SERVICE_NAME, OTEL_TRACES_SAMPLER,
// OTEL_TRACES_SAMPLER_ARG, OTEL_SDK_DISABLED) or by the [telemetry] section
// of the client and pool configuration; environment variables win.
//
// The OTLP exporter speaks OTLP/HTTP with the JSON encoding (see otlp.go):
// the official exporter links gRPC and protobuf (about 9 MB), while every
// OTLP/HTTP receiver (OpenTelemetry Collector, Jaeger, Tempo, Honeycomb, ...)
// accepts JSON on the same /v1/traces path.
//
// Attributes never carry secrets: no tokens, no environment, and of the
// arguments of a tool only the subcommand and flag names (see Args).
package tracing

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/cloudsourced/internal/config"
)

// Default service names of the components.
const (
	ServiceProxy        = "cloudsourced-proxy"
	ServiceDaemon       = "cloudsourced-daemon"
	ServiceControlPlane = "cloudsourced-controlplane"
	ServiceWorker       = "cloudsourced-worker"
	ServiceSupervisor   = "cloudsourced-supervisor"
)

// Exporters.
const (
	ExporterNone    = "none"
	ExporterOTLP    = "otlp"
	ExporterConsole = "console"
)

// EnvMachineEndpoint overrides the OTLP endpoint forwarded to machines
// (telemetry.machine_endpoint), e.g. when the collector is reachable from
// the machines under another address than from the orchestrator.
const EnvMachineEndpoint = "CLOUDSOURCED_OTEL_MACHINE_ENDPOINT"

// EnvConsoleFile makes the console exporter append to a file instead of
// writing to stderr.
const EnvConsoleFile = "CLOUDSOURCED_OTEL_CONSOLE_FILE"

const scopeName = "github.com/traumventures/cloudsourced"

// Options is the effective tracing configuration of a process.
type Options struct {
	Service  string            // service.name
	Exporter string            // otlp | console | none
	Endpoint string            // OTLP/HTTP traces URL (…/v1/traces)
	Headers  map[string]string // OTLP request headers (resolved values)
	Sampling float64           // parent based trace id ratio (default 1)
	// MachineEndpoint is the OTLP base URL forwarded to machines ("" =
	// Endpoint's base).
	MachineEndpoint string
	Compression     string // "" | gzip
	Timeout         time.Duration
	samplerFromEnv  bool
	baseEndpoint    string
}

// Enabled reports whether spans are exported.
func (o Options) Enabled() bool { return o.Exporter == ExporterOTLP || o.Exporter == ExporterConsole }

// Resolve combines the [telemetry] configuration with the standard
// environment variables (which win). dir resolves header references.
func Resolve(service string, c config.Telemetry, dir string) (Options, error) {
	o := Options{Service: service, Sampling: 1, Timeout: 10 * time.Second}
	if c.ServiceName != "" {
		o.Service = c.ServiceName
	}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		o.Service = v
	}
	o.Exporter = strings.ToLower(c.Exporter)
	base := c.Endpoint
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		base = v
	}
	traces := ""
	if base != "" {
		traces = strings.TrimRight(base, "/") + "/v1/traces"
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); v != "" {
		traces = v
		if base == "" {
			base = strings.TrimSuffix(strings.TrimRight(v, "/"), "/v1/traces")
		}
	}
	if o.Exporter == "" && traces != "" {
		o.Exporter = ExporterOTLP
	}
	if v := os.Getenv("OTEL_TRACES_EXPORTER"); v != "" {
		o.Exporter = strings.ToLower(strings.TrimSpace(strings.Split(v, ",")[0]))
	}
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") || o.Exporter == "" {
		o.Exporter = ExporterNone
	}
	switch o.Exporter {
	case ExporterNone, ExporterConsole:
	case ExporterOTLP:
		if traces == "" {
			base = "http://localhost:4318"
			traces = base + "/v1/traces"
		}
	default:
		return o, fmt.Errorf("telemetry: unsupported traces exporter %q (otlp, console, none)", o.Exporter)
	}
	o.Endpoint, o.baseEndpoint = traces, base
	o.Headers = map[string]string{}
	for name, ref := range c.Headers {
		v, err := config.ResolveRef(ref, dir)
		if err != nil {
			return o, fmt.Errorf("telemetry.headers.%s: %w", name, err)
		}
		o.Headers[name] = v
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
		for name, v := range parseHeaders(os.Getenv(k)) {
			o.Headers[name] = v
		}
	}
	if c.Sampling != nil {
		o.Sampling = *c.Sampling
	}
	o.samplerFromEnv = os.Getenv("OTEL_TRACES_SAMPLER") != ""
	o.MachineEndpoint = c.MachineEndpoint
	if v := os.Getenv(EnvMachineEndpoint); v != "" {
		o.MachineEndpoint = v
	}
	o.Compression = strings.ToLower(firstEnv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "OTEL_EXPORTER_OTLP_COMPRESSION"))
	if v := firstEnv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			o.Timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return o, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// parseHeaders parses "k1=v1,k2=v2" (values URL-encoded as in the spec).
func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = unescape(strings.TrimSpace(v))
	}
	return out
}

func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var (
	mu       sync.Mutex
	active   Options
	provider *sdktrace.TracerProvider
)

func init() {
	// Propagation works even when this process does not export, so that
	// a trace started upstream continues downstream.
	otel.SetTextMapPropagator(propagation.TraceContext{})
}

// Setup installs the global tracer provider for the options. It returns a
// shutdown function that flushes pending spans (always non-nil). Errors
// are logged, never fatal: tracing must not break builds.
func Setup(o Options, log *slog.Logger) func(context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	active = o
	if !o.Enabled() {
		return func(context.Context) error { return nil }
	}
	var exp sdktrace.SpanExporter
	switch o.Exporter {
	case ExporterConsole:
		var w io.Writer = os.Stderr
		if p := os.Getenv(EnvConsoleFile); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				w = f
			}
		}
		e, err := stdouttrace.New(stdouttrace.WithWriter(w))
		if err != nil {
			if log != nil {
				log.Warn("telemetry: console exporter", "err", err)
			}
			return func(context.Context) error { return nil }
		}
		exp = e
	default:
		if p := strings.ToLower(firstEnv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")); p == "grpc" && log != nil {
			log.Warn("telemetry: OTLP/gRPC is not supported, exporting OTLP/HTTP (JSON)", "endpoint", o.Endpoint)
		}
		exp = NewOTLPExporter(o.Endpoint, o.Headers, o.Compression, o.Timeout, log)
	}
	res, err := resource.New(context.Background(),
		resource.WithAttributes(attribute.String("service.name", o.Service)),
		resource.WithFromEnv(),
		resource.WithAttributes(attribute.String("service.namespace", "cloudsourced")),
	)
	if err != nil {
		res = resource.NewSchemaless(attribute.String("service.name", o.Service))
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithBatcher(exp)}
	if !o.samplerFromEnv {
		opts = append(opts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.Sampling))))
	}
	provider = sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	tp := provider
	return func(ctx context.Context) error { return tp.Shutdown(ctx) }
}

// SetupFromConfig resolves the configuration and installs the provider;
// configuration errors are logged and leave tracing off.
func SetupFromConfig(service string, c config.Telemetry, dir string, log *slog.Logger) func(context.Context) error {
	o, err := Resolve(service, c, dir)
	if err != nil {
		if log != nil {
			log.Warn("telemetry disabled", "err", err)
		}
		return func(context.Context) error { return nil }
	}
	return Setup(o, log)
}

// Flush exports pending spans of the global provider (used before a
// process replaces itself with exec).
func Flush(timeout time.Duration) {
	mu.Lock()
	tp := provider
	mu.Unlock()
	if tp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tp.ForceFlush(ctx)
}

// Shutdown flushes and stops the global provider.
func Shutdown(timeout time.Duration) {
	mu.Lock()
	tp := provider
	provider = nil
	mu.Unlock()
	if tp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tp.Shutdown(ctx)
}

// Active returns the options installed by Setup.
func Active() Options {
	mu.Lock()
	defer mu.Unlock()
	return active
}

// Forward returns the environment that makes machines (servers, host
// agents) export to the same backend as this process. Secret headers are
// returned separately (ForwardSecret), so providers keep them out of
// labels and logs.
func Forward() map[string]string {
	o := Active()
	out := map[string]string{}
	if !o.Enabled() {
		return out
	}
	out["OTEL_TRACES_EXPORTER"] = o.Exporter
	if o.Exporter == ExporterOTLP {
		ep := o.MachineEndpoint
		if ep == "" {
			ep = o.baseEndpoint
		}
		out["OTEL_EXPORTER_OTLP_ENDPOINT"] = ep
		if o.Compression != "" {
			out["OTEL_EXPORTER_OTLP_COMPRESSION"] = o.Compression
		}
	}
	for _, k := range []string{"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_RESOURCE_ATTRIBUTES"} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	if !o.samplerFromEnv && o.Sampling < 1 {
		out["OTEL_TRACES_SAMPLER"] = "parentbased_traceidratio"
		out["OTEL_TRACES_SAMPLER_ARG"] = strconv.FormatFloat(o.Sampling, 'f', -1, 64)
	}
	return out
}

// ForwardSecret returns the OTLP headers for machines (they may hold API
// keys of the tracing backend).
func ForwardSecret() map[string]string {
	o := Active()
	if o.Exporter != ExporterOTLP || len(o.Headers) == 0 {
		return nil
	}
	var parts []string
	for k, v := range o.Headers {
		parts = append(parts, k+"="+escape(v))
	}
	return map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": strings.Join(parts, ",")}
}

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ',' || c == '=' || c == '%' || c < 0x20 || c > 0x7e {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Tracer returns the cloudsourced tracer of the global provider.
func Tracer() trace.Tracer { return otel.Tracer(scopeName) }

// Start starts a span with the cloudsourced tracer.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// StartLinked starts a new root span linked to the span in ctx (background
// work triggered by a request, e.g. machine creation).
func StartLinked(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	opts = append(opts, trace.WithNewRoot())
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	return Tracer().Start(ctx, name, opts...)
}

// End records err on span (if any) and ends it.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// TraceID returns the hex trace id of the span in ctx ("" if none).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
