package events

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestRotationAndRetention(t *testing.T) {
	dir := t.TempDir()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	// A stale file from long ago and an unrelated file.
	os.WriteFile(filepath.Join(dir, "events-2026-09-01.jsonl"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "other.json"), nil, 0o600)
	now := day(1)
	l := Open(dir, 3*24*time.Hour)
	l.SetNow(func() time.Time { return now })
	if got := names(t, dir); len(got) != 1 || got[0] != "other.json" {
		t.Fatalf("expired file not pruned at open: %v", got)
	}
	for d := 1; d <= 6; d++ {
		now = day(d)
		l.Append(Event{Type: TaskPlaced, Task: "t", WaitMs: int64(d)})
		l.Append(Event{Type: TaskEnd, Task: "t", Usage: &scheduler.Usage{PeakMemory: 1 << 20, Wall: time.Second}})
	}
	l.Close()
	got := names(t, dir)
	want := []string{"events-2026-11-03.jsonl", "events-2026-11-04.jsonl", "events-2026-11-05.jsonl", "events-2026-11-06.jsonl", "other.json"}
	if len(got) != len(want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files %v, want %v", got, want)
		}
	}
	evs, err := Read(dir, day(4), day(6))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || evs[0].WaitMs != 4 || evs[3].Usage == nil || evs[3].Usage.PeakMemory != 1<<20 {
		t.Fatalf("window read: %+v", evs)
	}
	all, _ := Read(dir, time.Time{}, time.Time{})
	if len(all) != 8 {
		t.Fatalf("read all: %d", len(all))
	}
	// Reopening appends to the day's file.
	l2 := Open(dir, 365*24*time.Hour) // prunes with the real clock
	l2.SetNow(func() time.Time { return day(6).Add(time.Hour) })
	l2.Append(Event{Type: MachineCreate, Machine: "m"})
	l2.Close()
	if all, _ = Read(dir, day(6), time.Time{}); len(all) != 3 || all[2].Type != MachineCreate {
		t.Fatalf("append after reopen: %+v", all)
	}
}

func TestSizeCapDropsAndNilLog(t *testing.T) {
	var nl *Log
	nl.Append(Event{Type: TaskEnd}) // must not panic
	dir := t.TempDir()
	l := Open(dir, 0)
	path := filepath.Join(dir, "events-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	f, _ := os.Create(path)
	f.Truncate(MaxFileSize - 10)
	f.Close()
	l.Append(Event{Type: TaskEnd, Task: "too-large-for-the-rest"})
	if l.Dropped() != 1 {
		t.Fatalf("dropped %d", l.Dropped())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"1.5d":                 now.Add(-36 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
		"2026-11-01":           time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"2026-11-01T10:00:00Z": time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-3d", "-1h"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
