package metrics

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func sampleRegistry() *Registry {
	r := NewRegistry()
	c := r.Counter("cs_requests_total", "Requests by path.", "path", "code")
	c.Inc("/v1/acquire", "200")
	c.Add(2, "/v1/acquire", "200")
	c.Inc("/v1/status", "401")
	c.Add(-5, "/v1/status", "401") // ignored
	g := r.Gauge("cs_queue_length", "Waiting tasks.")
	g.Set(3)
	g.Add(-1)
	h := r.Histogram("cs_wait_seconds", "Wait time.\nSecond line with \\ backslash.", []float64{1, 0.5, 5})
	for _, v := range []float64{0.2, 0.7, 0.7, 3, 100} {
		h.Observe(v)
	}
	r.Gauge("cs_unused", "Never set.", "x")
	r.Collect(func() []Family {
		return []Family{GaugeFamily("cs_machines", "Machines by state.",
			Sample{Labels: L("state", "running"), Value: 2},
			Sample{Labels: L("state", `we"ird\`+"\n"), Value: 1})}
	})
	return r
}

func TestTextGolden(t *testing.T) {
	var b bytes.Buffer
	if err := WriteText(&b, sampleRegistry().Gather()); err != nil {
		t.Fatal(err)
	}
	golden(t, "registry.txt", b.String())
}

// Aggregation: machine snapshots travel as JSON and are merged with the
// orchestrator's own families under a machine label.
func TestAggregateGolden(t *testing.T) {
	machine := func(id string, running float64, exits ...string) []Family {
		r := NewRegistry()
		r.Gauge("cs_server_sessions_running", "Running sessions.").Set(running)
		e := r.Counter("cs_server_exits_total", "Exits by code.", "code")
		for _, c := range exits {
			e.Inc(c)
		}
		r.Histogram("cs_server_session_seconds", "Session duration.", []float64{1, 10}).Observe(4)
		b, err := json.Marshal(r.Gather())
		if err != nil {
			t.Fatal(err)
		}
		var back []Family
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		return WithLabels(back, Label{"machine", id}, Label{"provider", "local"})
	}
	own := NewRegistry()
	own.Gauge("cs_queue_length", "Waiting tasks.").Set(0)
	merged := Merge(own.Gather(), machine("m2", 0, "0"), machine("m1", 1, "0", "0", "101"),
		[]Family{{Name: "cs_queue_length", Type: Counter}}, // conflicting type: dropped
		[]Family{{Name: "bad name", Type: Gauge}})
	var b bytes.Buffer
	WriteText(&b, merged)
	golden(t, "aggregate.txt", b.String())
	if strings.Contains(b.String(), "bad name") {
		t.Error("invalid family name written")
	}
}

func TestWithLabelsReplaces(t *testing.T) {
	f := WithLabels([]Family{{Name: "x", Type: Gauge, Samples: []Sample{{Labels: L("machine", "inner", "a", "b")}}}}, Label{"machine", "outer"})
	got := f[0].Samples[0].Labels
	if len(got) != 2 || got[0].Value != "outer" || got[1].Name != "a" {
		t.Fatalf("labels %+v", got)
	}
}

func TestRegisterPanics(t *testing.T) {
	r := NewRegistry()
	r.Counter("a_total", "", "x")
	r.Counter("a_total", "", "x") // same shape: fine
	for _, fn := range []func(){
		func() { r.Gauge("a_total", "", "x") },
		func() { r.Counter("a_total", "", "y") },
		func() { r.Counter("1bad", "") },
		func() { r.Histogram("h", "", nil, "le") },
		func() { r.Counter("a_total", "").Inc() },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("expected panic")
				}
			}()
			fn()
		}()
	}
}
