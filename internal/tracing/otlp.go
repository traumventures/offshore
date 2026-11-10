package tracing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// OTLPExporter exports spans as OTLP/HTTP with the JSON encoding
// (Content-Type application/json, POST to …/v1/traces).
type OTLPExporter struct {
	url      string
	headers  map[string]string
	gzip     bool
	client   *http.Client
	log      *slog.Logger
	mu       sync.Mutex
	stopped  bool
	warnOnce sync.Once
}

// NewOTLPExporter creates an exporter posting to url.
func NewOTLPExporter(url string, headers map[string]string, compression string, timeout time.Duration, log *slog.Logger) *OTLPExporter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &OTLPExporter{url: url, headers: headers, gzip: compression == "gzip", client: &http.Client{Timeout: timeout}, log: log}
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *OTLPExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	stopped := e.stopped
	e.mu.Unlock()
	if stopped || len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(EncodeOTLP(spans))
	if err != nil {
		return err
	}
	var rd io.Reader = bytes.NewReader(body)
	if e.gzip {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		rd = &b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			err = fmt.Errorf("otlp: %s: %s", e.url, resp.Status)
		}
	}
	if err != nil && e.log != nil {
		e.warnOnce.Do(func() { e.log.Warn("telemetry: span export failed", "err", err) })
	}
	return err
}

// Shutdown implements sdktrace.SpanExporter.
func (e *OTLPExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	return nil
}

// OTLP JSON schema (opentelemetry-proto, JSON mapping: ids as hex, 64 bit
// integers as strings, enums as numbers).

// TracesData is ExportTraceServiceRequest.
type TracesData struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

// ResourceSpans groups spans of one resource.
type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

// Resource describes the producing entity.
type Resource struct {
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// ScopeSpans groups spans of one instrumentation scope.
type ScopeSpans struct {
	Scope Scope  `json:"scope"`
	Spans []Span `json:"spans"`
}

// Scope is the instrumentation scope.
type Scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Span is one OTLP span.
type Span struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	TraceState   string     `json:"traceState,omitempty"`
	ParentSpanID string     `json:"parentSpanId,omitempty"`
	Flags        uint32     `json:"flags,omitempty"`
	Name         string     `json:"name"`
	Kind         int        `json:"kind"`
	Start        string     `json:"startTimeUnixNano"`
	End          string     `json:"endTimeUnixNano"`
	Attributes   []KeyValue `json:"attributes,omitempty"`
	Events       []Event    `json:"events,omitempty"`
	Links        []Link     `json:"links,omitempty"`
	Status       Status     `json:"status"`
}

// Event is a span event.
type Event struct {
	Time       string     `json:"timeUnixNano"`
	Name       string     `json:"name"`
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// Link references another span.
type Link struct {
	TraceID    string     `json:"traceId"`
	SpanID     string     `json:"spanId"`
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// Status is the span status (0 unset, 1 ok, 2 error).
type Status struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// KeyValue is an attribute.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is an attribute value.
type AnyValue struct {
	String *string     `json:"stringValue,omitempty"`
	Bool   *bool       `json:"boolValue,omitempty"`
	Int    *string     `json:"intValue,omitempty"`
	Double *float64    `json:"doubleValue,omitempty"`
	Array  *ArrayValue `json:"arrayValue,omitempty"`
}

// ArrayValue is a list of values.
type ArrayValue struct {
	Values []AnyValue `json:"values"`
}

func nanos(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func anyValue(v attribute.Value) AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return AnyValue{Bool: &b}
	case attribute.INT64:
		s := strconv.FormatInt(v.AsInt64(), 10)
		return AnyValue{Int: &s}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return AnyValue{Double: &f}
	case attribute.BOOLSLICE:
		var vs []AnyValue
		for _, b := range v.AsBoolSlice() {
			vs = append(vs, anyValue(attribute.BoolValue(b)))
		}
		return AnyValue{Array: &ArrayValue{Values: vs}}
	case attribute.INT64SLICE:
		var vs []AnyValue
		for _, n := range v.AsInt64Slice() {
			vs = append(vs, anyValue(attribute.Int64Value(n)))
		}
		return AnyValue{Array: &ArrayValue{Values: vs}}
	case attribute.FLOAT64SLICE:
		var vs []AnyValue
		for _, f := range v.AsFloat64Slice() {
			vs = append(vs, anyValue(attribute.Float64Value(f)))
		}
		return AnyValue{Array: &ArrayValue{Values: vs}}
	case attribute.STRINGSLICE:
		vs := []AnyValue{}
		for _, s := range v.AsStringSlice() {
			vs = append(vs, anyValue(attribute.StringValue(s)))
		}
		return AnyValue{Array: &ArrayValue{Values: vs}}
	}
	s := v.Emit()
	return AnyValue{String: &s}
}

func keyValues(kvs []attribute.KeyValue) []KeyValue {
	out := make([]KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, KeyValue{Key: string(kv.Key), Value: anyValue(kv.Value)})
	}
	return out
}

// EncodeOTLP converts finished spans to the OTLP JSON request.
func EncodeOTLP(spans []sdktrace.ReadOnlySpan) TracesData {
	type scopeKey struct {
		res   *resource.Resource
		scope instrumentation.Scope
	}
	var resOrder []*resource.Resource
	byRes := map[*resource.Resource][]instrumentation.Scope{}
	byScope := map[scopeKey][]Span{}
	for _, s := range spans {
		res, sc := s.Resource(), s.InstrumentationScope()
		k := scopeKey{res, sc}
		if _, ok := byRes[res]; !ok {
			resOrder = append(resOrder, res)
		}
		if _, ok := byScope[k]; !ok {
			byRes[res] = append(byRes[res], sc)
		}
		byScope[k] = append(byScope[k], encodeSpan(s))
	}
	var td TracesData
	for _, res := range resOrder {
		rs := ResourceSpans{}
		if res != nil {
			rs.Resource.Attributes = keyValues(res.Attributes())
		}
		for _, sc := range byRes[res] {
			rs.ScopeSpans = append(rs.ScopeSpans, ScopeSpans{Scope: Scope{Name: sc.Name, Version: sc.Version}, Spans: byScope[scopeKey{res, sc}]})
		}
		td.ResourceSpans = append(td.ResourceSpans, rs)
	}
	return td
}

func encodeSpan(s sdktrace.ReadOnlySpan) Span {
	sc := s.SpanContext()
	out := Span{
		TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), TraceState: sc.TraceState().String(),
		Flags: uint32(sc.TraceFlags()), Name: s.Name(), Kind: int(s.SpanKind()),
		Start: nanos(s.StartTime()), End: nanos(s.EndTime()), Attributes: keyValues(s.Attributes()),
	}
	if p := s.Parent(); p.IsValid() {
		out.ParentSpanID = p.SpanID().String()
	}
	for _, ev := range s.Events() {
		out.Events = append(out.Events, Event{Time: nanos(ev.Time), Name: ev.Name, Attributes: keyValues(ev.Attributes)})
	}
	for _, l := range s.Links() {
		out.Links = append(out.Links, Link{TraceID: l.SpanContext.TraceID().String(), SpanID: l.SpanContext.SpanID().String(), Attributes: keyValues(l.Attributes)})
	}
	switch st := s.Status(); st.Code {
	case codes.Ok:
		out.Status = Status{Code: 1}
	case codes.Error:
		out.Status = Status{Code: 2, Message: st.Description}
	}
	return out
}
