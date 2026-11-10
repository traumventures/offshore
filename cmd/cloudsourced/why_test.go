package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

func TestPrintRun(t *testing.T) {
	code := 101
	r := events.Run{
		Time: time.Now(), Tool: "cargo", Args: []string{"build"}, Mode: events.Remote, Why: "[tool.cargo] allows remote execution",
		Machine: "abc123", Provider: "aws", Size: "c7a", Price: 0.25, Placement: "warm build slot for repository",
		Slot: "s1", Warm: true, Exit: &code, Ports: []string{"8080"},
		Timings: events.Timings{Queue: 1500, Run: 2000, Total: 4000},
		Usage:   &scheduler.Usage{PeakMemory: 3 << 30, CPUSeconds: 12.5},
		Cache:   map[string]int64{"sccache_misses": 2, "sccache_hits": 40},
	}
	var b bytes.Buffer
	printRun(&b, r)
	out := b.String()
	for _, want := range []string{"cargo build", "remote: [tool.cargo]", "abc123 (aws/c7a, 0.250/h)", "placement  warm build slot",
		"s1 (warm)", "exit       101", "queue / machine start", "1.5s", "peak memory 3.0G, cpu 12.5s", "sccache_hits=40 sccache_misses=2", "8080"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestWhyReadsRunLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOUDSOURCED_STATE_DIR", dir)
	if err := why(nil); err == nil {
		t.Fatal("expected error without runs")
	}
	events.Append(dir, events.Run{Tool: "a", Mode: events.Local, Why: "x"})
	events.Append(dir, events.Run{Tool: "b", Mode: events.Local, Why: "y"})
	if err := why([]string{"2"}); err != nil {
		t.Fatal(err)
	}
	if err := why([]string{"3"}); err == nil {
		t.Fatal("expected error for missing run")
	}
	if err := why([]string{"bogus"}); err == nil {
		t.Fatal("expected usage error")
	}
}
