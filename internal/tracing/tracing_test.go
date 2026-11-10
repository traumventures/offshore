package tracing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/traumventures/cloudsourced/internal/config"
)

func clearOTELEnv(t *testing.T) {
	for _, k := range []string{"OTEL_SERVICE_NAME", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_TRACES_EXPORTER", "OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", EnvMachineEndpoint, "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(k, "")
	}
}

func TestResolveDefaultsOff(t *testing.T) {
	clearOTELEnv(t)
	o, err := Resolve(ServiceProxy, config.Telemetry{}, "")
	if err != nil || o.Enabled() || o.Service != ServiceProxy {
		t.Fatalf("default must be off: %+v %v", o, err)
	}
}

func TestResolveConfigAndEnvPrecedence(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("CS_TEST_KEY", "s3cret")
	ratio := 0.25
	c := config.Telemetry{Endpoint: "http://cfg:4318", Headers: map[string]string{"x-key": "env:CS_TEST_KEY"}, Sampling: &ratio, MachineEndpoint: "http://10.0.0.1:4318"}
	o, err := Resolve(ServiceDaemon, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if o.Exporter != ExporterOTLP || o.Endpoint != "http://cfg:4318/v1/traces" || o.Headers["x-key"] != "s3cret" || o.Sampling != 0.25 {
		t.Fatalf("config not applied: %+v", o)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://env:4318/")
	t.Setenv("OTEL_SERVICE_NAME", "custom")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abc,x-key=other")
	o, _ = Resolve(ServiceDaemon, c, "")
	if o.Endpoint != "http://env:4318/v1/traces" || o.Service != "custom" || o.Headers["authorization"] != "Bearer abc" || o.Headers["x-key"] != "other" {
		t.Fatalf("environment must win: %+v", o)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://traces:9/custom")
	o, _ = Resolve(ServiceDaemon, c, "")
	if o.Endpoint != "http://traces:9/custom" {
		t.Fatalf("traces endpoint used as is: %s", o.Endpoint)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	if o, _ = Resolve(ServiceDaemon, c, ""); o.Enabled() {
		t.Fatal("OTEL_TRACES_EXPORTER=none must disable")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	if o, _ = Resolve(ServiceDaemon, config.Telemetry{}, ""); o.Exporter != ExporterConsole {
		t.Fatalf("console: %+v", o)
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if o, _ = Resolve(ServiceDaemon, config.Telemetry{}, ""); o.Enabled() {
		t.Fatal("OTEL_SDK_DISABLED must disable")
	}
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "zipkin")
	if _, err := Resolve(ServiceDaemon, config.Telemetry{}, ""); err == nil {
		t.Fatal("unsupported exporter accepted")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if o, _ = Resolve(ServiceDaemon, config.Telemetry{}, ""); o.Endpoint != "http://localhost:4318/v1/traces" {
		t.Fatalf("otlp default endpoint: %s", o.Endpoint)
	}
}

func TestArgsDropValues(t *testing.T) {
	sub, flags := Args([]string{"+nightly", "--token=abc", "-j8", "test", "secret-positional", "--release", "-p", "pkg", "--", "--x"})
	if sub != "test" {
		t.Fatalf("subcommand %q", sub)
	}
	want := []string{"--token", "-j", "--release", "-p"}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags %v, want %v", flags, want)
	}
	for _, f := range flags {
		if strings.Contains(f, "abc") || strings.Contains(f, "8") {
			t.Fatalf("value leaked: %v", flags)
		}
	}
}

func TestInjectExtractRoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	defer tp.Shutdown(context.Background())
	ctx, span := tp.Tracer("t").Start(context.Background(), "root")
	defer span.End()
	tp1 := Inject(ctx)
	if !strings.HasPrefix(tp1, "00-"+span.SpanContext().TraceID().String()+"-"+span.SpanContext().SpanID().String()) {
		t.Fatalf("traceparent %q", tp1)
	}
	got := trace.SpanContextFromContext(Extract(context.Background(), tp1))
	if !got.IsRemote() || got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("extracted %+v", got)
	}
	if Inject(context.Background()) != "" {
		t.Fatal("no span, no traceparent")
	}
	if sc := trace.SpanContextFromContext(Extract(context.Background(), "garbage")); sc.IsValid() {
		t.Fatal("malformed traceparent accepted")
	}
	h := http.Header{}
	InjectHTTP(ctx, h)
	if h.Get("Traceparent") != tp1 {
		t.Fatalf("header %v", h)
	}
	if sc := trace.SpanContextFromContext(ExtractHTTP(context.Background(), h)); sc.TraceID() != span.SpanContext().TraceID() {
		t.Fatal("http extract")
	}
}

// TestOTLPExporter checks the OTLP/HTTP JSON encoding against a collector
// stub: resource, ids as hex, parent, attributes, events, links, status.
func TestOTLPExporter(t *testing.T) {
	var mu sync.Mutex
	var bodies []TracesData
	var hdr http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var td TracesData
		if err := json.Unmarshal(b, &td); err != nil {
			t.Errorf("invalid json: %v", err)
		}
		mu.Lock()
		bodies, hdr = append(bodies, td), r.Header.Clone()
		mu.Unlock()
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
	}))
	defer ts.Close()
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", ts.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=k1")
	o, err := Resolve(ServiceWorker, config.Telemetry{}, "")
	if err != nil {
		t.Fatal(err)
	}
	prev := otel.GetTracerProvider()
	shutdown := Setup(o, nil)
	defer otel.SetTracerProvider(prev)
	ctx, root := Start(context.Background(), "root", trace.WithAttributes(attribute.Int64("n", 42), attribute.StringSlice("flags", []string{"-j"})))
	_, child := Start(ctx, "child")
	child.AddEvent("ev", trace.WithAttributes(attribute.Bool("b", true)))
	End(child, io.ErrUnexpectedEOF)
	_, linked := StartLinked(ctx, "linked")
	linked.End()
	root.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hdr.Get("X-Api-Key") != "k1" {
		t.Fatalf("headers %v", hdr)
	}
	spans := map[string]Span{}
	for _, td := range bodies {
		for _, rs := range td.ResourceSpans {
			svc := ""
			for _, kv := range rs.Resource.Attributes {
				if kv.Key == "service.name" && kv.Value.String != nil {
					svc = *kv.Value.String
				}
			}
			if svc != ServiceWorker {
				t.Fatalf("service.name %q", svc)
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					spans[s.Name] = s
				}
			}
		}
	}
	r, c, l := spans["root"], spans["child"], spans["linked"]
	if len(r.TraceID) != 32 || len(r.SpanID) != 16 || r.ParentSpanID != "" {
		t.Fatalf("root %+v", r)
	}
	if c.TraceID != r.TraceID || c.ParentSpanID != r.SpanID || c.Status.Code != 2 || len(c.Events) == 0 {
		t.Fatalf("child %+v", c)
	}
	if l.TraceID == r.TraceID || len(l.Links) != 1 || l.Links[0].SpanID != r.SpanID {
		t.Fatalf("linked span must be a new root linked to root: %+v", l)
	}
	var n *string
	for _, kv := range r.Attributes {
		if kv.Key == "n" {
			n = kv.Value.Int
		}
	}
	if n == nil || *n != "42" {
		t.Fatalf("int attribute encoded as string: %+v", r.Attributes)
	}
	if r.Start == "0" || r.End == "0" {
		t.Fatalf("times %s %s", r.Start, r.End)
	}
}

func TestForwardToMachines(t *testing.T) {
	clearOTELEnv(t)
	defer Setup(Options{Exporter: ExporterNone}, nil)
	prev := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prev)
	o := Options{Service: ServiceDaemon, Exporter: ExporterOTLP, Endpoint: "http://a:4318/v1/traces", baseEndpoint: "http://a:4318",
		Headers: map[string]string{"x-key": "v,1"}, Sampling: 0.5, MachineEndpoint: "http://gw:4318", Timeout: time.Second}
	shutdown := Setup(o, nil)
	defer shutdown(context.Background())
	env := Forward()
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://gw:4318" || env["OTEL_TRACES_EXPORTER"] != "otlp" ||
		env["OTEL_TRACES_SAMPLER"] != "parentbased_traceidratio" || env["OTEL_TRACES_SAMPLER_ARG"] != "0.5" {
		t.Fatalf("forward %v", env)
	}
	if _, ok := env["OTEL_EXPORTER_OTLP_HEADERS"]; ok {
		t.Fatal("headers must only travel as secrets")
	}
	sec := ForwardSecret()
	if got := parseHeaders(sec["OTEL_EXPORTER_OTLP_HEADERS"]); got["x-key"] != "v,1" {
		t.Fatalf("secret headers %v", sec)
	}
}
