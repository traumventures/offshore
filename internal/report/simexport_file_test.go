package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
)

// TestSimTraceFile: converting an event log file (prefiltered task
// events) gives the trace of converting all its events.
func TestSimTraceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events-2026-11-11.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i, ev := range fixture() {
		// Noise the converter ignores, interleaved.
		noise := events.Event{Time: ev.Time.Add(-time.Second), Type: events.MachineCreate, Machine: "m1", Reason: `"type":"task_end"`}
		if err := enc.Encode(noise); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.WriteString("not json\n")
		}
	}
	f.Close()
	all, err := events.Read(dir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	o := ExportOptions{ColdPenalty: 0.5}
	want := SimTrace(all, time.Time{}, time.Time{}, o)
	got, err := SimTraceFile(path, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Jobs) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("SimTraceFile = %+v, want %+v", got, want)
	}
}

// TestSimTraceFileTiming compares the prefiltered reader with a full
// decode of a real event log (OFFSHORE_EVENTS_FILE).
func TestSimTraceFileTiming(t *testing.T) {
	path := os.Getenv("OFFSHORE_EVENTS_FILE")
	if path == "" {
		t.Skip("set OFFSHORE_EVENTS_FILE")
	}
	t0 := time.Now()
	all, err := events.Read(filepath.Dir(path), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := SimTrace(all, time.Time{}, time.Time{}, ExportOptions{})
	full := time.Since(t0)
	t0 = time.Now()
	got, err := SimTraceFile(path, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d events, %d jobs: full decode %s, prefiltered %s", len(all), len(got.Jobs), full, time.Since(t0))
	if !reflect.DeepEqual(got, want) {
		t.Fatal("traces differ")
	}
}
