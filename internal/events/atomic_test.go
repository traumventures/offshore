package events

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakeFile writes only part of a line and fails, like a full disk.
type fakeFile struct {
	data      []byte
	short     int // bytes written before failing (-1: write all)
	truncated bool
}

func (f *fakeFile) Write(b []byte) (int, error) {
	if f.short < 0 {
		f.data = append(f.data, b...)
		return len(b), nil
	}
	f.data = append(f.data, b[:f.short]...)
	return f.short, syscall.ENOSPC
}

func (f *fakeFile) Truncate(n int64) error {
	f.data, f.truncated = f.data[:n], true
	return nil
}

func TestWriteLineUndoesShortWrite(t *testing.T) {
	f := &fakeFile{data: []byte("{\"a\":1}\n"), short: 3}
	err := writeLine(f, []byte("{\"b\":2}\n"), int64(len(f.data)))
	if !errors.Is(err, syscall.ENOSPC) || !f.truncated || string(f.data) != "{\"a\":1}\n" {
		t.Fatalf("err %v, data %q", err, f.data)
	}
	f.short = -1
	if err := writeLine(f, []byte("{\"c\":3}\n"), int64(len(f.data))); err != nil || string(f.data) != "{\"a\":1}\n{\"c\":3}\n" {
		t.Fatalf("err %v, data %q", err, f.data)
	}
}

func TestDroppedWarnsOnceAndReadersSkipGarbage(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "events-2026-11-02.jsonl")
	os.WriteFile(path, []byte("{\"time\":\"2026-11-02T10:00:00Z\",\"type\":\"task_end\"}\n{\"time\":\"2026-11-02T10:0\n\n"), 0o600)
	l := Open(dir, 0)
	l.SetNow(func() time.Time { return day })
	warns := 0
	l.SetWarn(func(string, error) { warns++ })
	l.Append(Event{Type: TaskPlaced})
	evs, skipped, err := ReadCounted(dir, time.Time{}, time.Time{})
	if err != nil || len(evs) != 2 || skipped != 1 {
		t.Fatalf("events %d, skipped %d, err %v", len(evs), skipped, err)
	}
	// A log in a directory that cannot exist drops and warns once.
	bad := Open(filepath.Join(path, "sub"), 0)
	bad.SetWarn(func(string, error) { warns++ })
	bad.Append(Event{Type: TaskPlaced})
	bad.Append(Event{Type: TaskPlaced})
	if bad.Dropped() != 2 || warns != 1 {
		t.Fatalf("dropped %d warns %d", bad.Dropped(), warns)
	}
	if SkippedLines() < 1 {
		t.Fatal("skipped lines not counted")
	}
}
