package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// Gauge is one gauge data point for ExportGauges.
type Gauge struct {
	Name  string
	Unit  string
	Value float64
	Time  time.Time
	Attrs []attribute.KeyValue
}

var gaugeWarn sync.Once

// ExportGauges sends gauge points as OTLP/HTTP JSON metrics to the
// configured backend (…/v1/metrics next to …/v1/traces) in the
// background. It does nothing unless the OTLP exporter is active.
func ExportGauges(points []Gauge) {
	o := Active()
	if o.Exporter != ExporterOTLP || len(points) == 0 || o.Endpoint == "" {
		return
	}
	url := strings.TrimSuffix(o.Endpoint, "/v1/traces") + "/v1/metrics"
	body, err := json.Marshal(EncodeGauges(o.Service, points))
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range o.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("otlp: %s: %s", url, resp.Status)
			}
		}
		if err != nil {
			gaugeWarn.Do(func() {
				if l := exportLog(); l != nil {
					l.Warn("telemetry: metric export failed", "err", err)
				}
			})
		}
	}()
}

// MetricsData is ExportMetricsServiceRequest (gauges only).
type MetricsData struct {
	ResourceMetrics []ResourceMetrics `json:"resourceMetrics"`
}

// ResourceMetrics groups metrics of one resource.
type ResourceMetrics struct {
	Resource     Resource       `json:"resource"`
	ScopeMetrics []ScopeMetrics `json:"scopeMetrics"`
}

// ScopeMetrics groups metrics of one scope.
type ScopeMetrics struct {
	Scope   Scope    `json:"scope"`
	Metrics []Metric `json:"metrics"`
}

// Metric is one named gauge.
type Metric struct {
	Name  string    `json:"name"`
	Unit  string    `json:"unit,omitempty"`
	Gauge GaugeData `json:"gauge"`
}

// GaugeData holds the points of a gauge.
type GaugeData struct {
	DataPoints []DataPoint `json:"dataPoints"`
}

// DataPoint is one gauge value.
type DataPoint struct {
	Attributes []KeyValue `json:"attributes,omitempty"`
	Time       string     `json:"timeUnixNano"`
	AsDouble   float64    `json:"asDouble"`
}

// EncodeGauges groups points by metric name.
func EncodeGauges(service string, points []Gauge) MetricsData {
	var ms []Metric
	idx := map[string]int{}
	for _, p := range points {
		i, ok := idx[p.Name]
		if !ok {
			i = len(ms)
			idx[p.Name] = i
			ms = append(ms, Metric{Name: p.Name, Unit: p.Unit})
		}
		ms[i].Gauge.DataPoints = append(ms[i].Gauge.DataPoints, DataPoint{Attributes: keyValues(p.Attrs), Time: nanos(p.Time), AsDouble: p.Value})
	}
	return MetricsData{ResourceMetrics: []ResourceMetrics{{
		Resource:     Resource{Attributes: keyValues([]attribute.KeyValue{attribute.String("service.name", service), attribute.String("service.namespace", "offshore")})},
		ScopeMetrics: []ScopeMetrics{{Scope: Scope{Name: scopeName}, Metrics: ms}},
	}}}
}
