package worker

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/metrics"
	"github.com/traumventures/offshore/internal/proto"
)

func TestStatusCarriesMetrics(t *testing.T) {
	s, err := New(Config{MachineID: "m1", CacheDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.metrics.sessionEnded(0, 3, false, map[string]int64{"sccache_hits": 5, "sccache_misses": 2, "Bad Key": 1})
	s.metrics.sessionEnded(137, 30, true, nil)
	s.countBytes(1024, transferFetch)
	s.metrics.rejected.Inc("draining")
	// The snapshot survives the JSON status stream.
	b, err := json.Marshal(s.Status())
	if err != nil {
		t.Fatal(err)
	}
	var st proto.Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	metrics.WriteText(&text, st.Metrics)
	for _, want := range []string{
		`offshore_worker_session_exits_total{code="0"} 1`,
		`offshore_worker_session_exits_total{code="137"} 1`,
		`offshore_worker_oom_kills_total 1`,
		`offshore_worker_compiler_cache_total{counter="sccache_hits"} 5`,
		`offshore_worker_compiler_cache_total{counter="sccache_misses"} 2`,
		`offshore_worker_transfer_bytes_total{direction="fetch"} 1024`,
		`offshore_worker_transfer_bytes_total{direction="output"} 0`,
		`offshore_worker_sessions_rejected_total{reason="draining"} 1`,
		`offshore_worker_session_duration_seconds_count 2`,
		`offshore_worker_session_duration_seconds_sum 33`,
		`offshore_worker_sessions_running 0`,
		`offshore_worker_ready 0`,
		`offshore_worker_pressure{resource="cpu"}`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q in\n%s", want, text.String())
		}
	}
	if strings.Contains(text.String(), "Bad Key") {
		t.Error("unbounded cache counter name exported")
	}
}
