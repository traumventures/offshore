package worker

import (
	"strconv"
	"strings"

	"github.com/traumventures/cloudsourced/internal/metrics"
)

// serverMetrics are the worker's metrics. They travel in the
// status stream (probe scope) and the orchestrator republishes them with a
// machine label; the server opens no extra port.
type serverMetrics struct {
	reg      *metrics.Registry
	exits    metrics.CounterVec
	duration metrics.HistogramVec
	oom      metrics.CounterVec
	cache    metrics.CounterVec
	bytes    metrics.CounterVec
	rejected metrics.CounterVec
}

// Transfer directions of cloudsourced_worker_transfer_bytes_total.
const (
	transferFetch     = "fetch"     // source blobs fetched from the client (FUSE)
	transferWriteback = "writeback" // changed files sent back
	transferOutput    = "output"    // output files sent to the client (eager, fetch, output mount)
)

func newServerMetrics(s *Worker) *serverMetrics {
	r := metrics.NewRegistry()
	m := &serverMetrics{
		reg:      r,
		exits:    r.Counter("cloudsourced_worker_session_exits_total", "Finished sessions by exit code (128+N for signal N).", "code"),
		duration: r.Histogram("cloudsourced_worker_session_duration_seconds", "Wall time of finished sessions.", metrics.DurationBuckets),
		oom:      r.Counter("cloudsourced_worker_oom_kills_total", "Sessions killed for exceeding their memory limit."),
		cache:    r.Counter("cloudsourced_worker_compiler_cache_total", "Compiler cache counters summed over sessions (e.g. sccache_hits, sccache_misses).", "counter"),
		bytes:    r.Counter("cloudsourced_worker_transfer_bytes_total", "Bytes transferred between machine and client daemon.", "direction"),
		rejected: r.Counter("cloudsourced_worker_sessions_rejected_total", "Sessions refused before start.", "reason"),
	}
	for _, d := range []string{transferFetch, transferWriteback, transferOutput} {
		m.bytes.Add(0, d)
	}
	m.oom.Add(0)
	r.Collect(func() []metrics.Family {
		st := s.statusOnly()
		ready := 0.0
		if st.Ready {
			ready = 1
		}
		draining := 0.0
		if st.Draining {
			draining = 1
		}
		return []metrics.Family{
			metrics.GaugeFamily("cloudsourced_worker_sessions_running", "Sessions running now.", metrics.Sample{Value: float64(len(st.Sessions))}),
			metrics.GaugeFamily("cloudsourced_worker_pressure", "PSI some avg10 of the machine (0..1).",
				metrics.Sample{Labels: metrics.L("resource", "cpu"), Value: st.Pressure.CPU},
				metrics.Sample{Labels: metrics.L("resource", "memory"), Value: st.Pressure.Memory}),
			metrics.GaugeFamily("cloudsourced_worker_reserved", "Resources reserved by running sessions (CPU cores, memory bytes).",
				metrics.Sample{Labels: metrics.L("resource", "cpu"), Value: st.UsedCPU},
				metrics.Sample{Labels: metrics.L("resource", "memory"), Value: float64(st.UsedMemory)}),
			metrics.GaugeFamily("cloudsourced_worker_memory_used_bytes", "Measured memory of running sessions.", metrics.Sample{Value: float64(st.Measured.Memory)}),
			metrics.GaugeFamily("cloudsourced_worker_ready", "1 when the machine is provisioned and accepts sessions.", metrics.Sample{Value: ready}),
			metrics.GaugeFamily("cloudsourced_worker_draining", "1 while the machine drains before expiry.", metrics.Sample{Value: draining}),
			metrics.GaugeFamily("cloudsourced_worker_warm_repos", "Repositories with unexpired build slots.", metrics.Sample{Value: float64(len(st.WarmRepos))}),
		}
	})
	return m
}

// sessionEnded records a finished session.
func (m *serverMetrics) sessionEnded(code int, seconds float64, oom bool, cache map[string]int64) {
	m.exits.Inc(strconv.Itoa(code))
	m.duration.Observe(seconds)
	if oom {
		m.oom.Inc()
	}
	for k, v := range cache {
		if v > 0 && validCounter(k) {
			m.cache.Add(float64(v), k)
		}
	}
}

// validCounter bounds label values of cache counters to simple names.
func validCounter(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	return strings.Trim(k, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

// countBytes records a transfer (servers built without New have no metrics).
func (s *Worker) countBytes(n float64, direction string) {
	if s.metrics != nil {
		s.metrics.bytes.Add(n, direction)
	}
}
