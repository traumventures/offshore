// Package events is the orchestrator's analysis event log: one JSON object
// per line in daily files (events-YYYY-MM-DD.jsonl, UTC) in the state
// directory, removed after a retention period. It is always on and
// independent of tracing; `cloudsourced report` and `cloudsourced sim
// export` read it.
//
// Task events describe sessions (arrival and placement, end with measured
// usage); machine events describe the pool (create, ready, start, stop,
// destroy, drain, observed state changes). Every event carries the fields
// needed to interpret it on its own, so readers need no other state.
package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Event types.
const (
	// TaskPlaced: a request was decided (Placement: assign, reject,
	// timeout, canceled, error), with arrival time and queue wait.
	TaskPlaced = "task_placed"
	// TaskEnd: a session finished and its usage was reported.
	TaskEnd = "task_end"

	MachineCreate  = "machine_create"  // creation requested (capacity on its way)
	MachineReady   = "machine_ready"   // server reported ready
	MachineStart   = "machine_start"   // start of a stopped machine requested
	MachineStop    = "machine_stop"    // stopped (disk kept)
	MachineDestroy = "machine_destroy" // destroyed
	MachineDrain   = "machine_drain"   // no new sessions
	MachineUndrain = "machine_undrain" // accepts sessions again
	// MachineState: a state change observed from the provider (e.g. a
	// machine stopped itself when idle, or disappeared).
	MachineState = "machine_state"
	// MachineSample: periodic utilisation of a running machine (every
	// SampleInterval): reservations, measured usage
	// (busy cores, session memory), pressure and session count.
	MachineSample = "machine_sample"

	// SnapshotSave and SnapshotRestore: a build slot was saved to or
	// restored from an object store (Repo, Slot, Bytes, Chunks, NewChunks,
	// Seconds, Store, Provider, Machine).
	SnapshotSave    = "snapshot_save"
	SnapshotRestore = "snapshot_restore"
	// SlotAction: the scheduler handled a full or growing build slot
	// (Reason: resize, prune, move, cold; Repo, Machine, Bytes).
	SlotAction = "slot_action"
	// ProviderDown: a provider reported no capacity or quota; it gets no
	// new machines for Seconds (cooldown with backoff).
	ProviderDown = "provider_down"
)

// SampleInterval is how often a running machine is sampled.
const SampleInterval = 10 * time.Second

// Event is one line of the log. Zero fields are omitted.
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"`

	// Task fields.
	Task    string               `json:"task,omitempty"` // lease id
	Repo    string               `json:"repo,omitempty"` // repository id
	Env     string               `json:"env,omitempty"`  // environment hash
	Key     string               `json:"key,omitempty"`  // learning key (repo/tool/subcommand)
	Arrival time.Time            `json:"arrival,omitzero"`
	WaitMs  int64                `json:"wait_ms,omitempty"`
	Need    *scheduler.Resources `json:"need,omitempty"`    // estimate used for packing
	Limit   *scheduler.Resources `json:"limit,omitempty"`   // hard ceiling
	Reserve *scheduler.Resources `json:"reserve,omitempty"` // granted reservation
	// Placement is the outcome of a request (assign, reject, timeout,
	// canceled, error); Reason the policy's explanation.
	Placement string           `json:"placement,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Usage     *scheduler.Usage `json:"usage,omitempty"`

	// Machine fields (also set on task events for the leased machine).
	Machine  string               `json:"machine,omitempty"`
	Provider string               `json:"provider,omitempty"`
	Size     string               `json:"size,omitempty"`
	Price    float64              `json:"price_per_hour,omitempty"`
	Capacity *scheduler.Resources `json:"capacity,omitempty"`
	State    string               `json:"state,omitempty"` // MachineState: new provider state
	// LatencyMs is the provider call duration of a machine action.
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`

	// MachineSample fields.
	Reserved *scheduler.Resources `json:"reserved,omitempty"` // sum of session reservations
	Used     *scheduler.Resources `json:"used,omitempty"`     // busy cores, memory of sessions
	Pressure *scheduler.Pressure  `json:"pressure,omitempty"` // PSI some avg10, 0..1
	Sessions int                  `json:"sessions,omitempty"`
	Disk     *Disk                `json:"disk,omitempty"` // machine disk usage

	// Snapshot fields (SnapshotSave, SnapshotRestore, SlotAction).
	Slot      string  `json:"slot,omitempty"`
	Store     string  `json:"store,omitempty"`
	Bytes     int64   `json:"bytes,omitempty"`
	Chunks    int64   `json:"chunks,omitempty"`
	NewChunks int64   `json:"new_chunks,omitempty"`
	Seconds   float64 `json:"seconds,omitempty"`

	// Labels describe who ran a task (worktree, repo, branch, user, agent
	// and free labels; see internal/labels). Set on task events.
	Labels map[string]string `json:"labels,omitempty"`

	// TraceID correlates the event with a distributed trace.
	TraceID string `json:"trace_id,omitempty"`
}

// Disk is a machine's disk usage in bytes.
type Disk struct {
	Total         int64 `json:"total"`
	Used          int64 `json:"used"`
	Slots         int64 `json:"slots"`
	MachineCaches int64 `json:"machine_caches"`
}

// DefaultRetention is how long daily files are kept.
const DefaultRetention = 30 * 24 * time.Hour

// MaxFileSize bounds one daily file; further events of the day are dropped
// (and counted in Dropped).
const MaxFileSize = 256 << 20

const prefix, suffix = "events-", ".jsonl"

// Log appends events to daily files. A nil *Log discards events.
type Log struct {
	dir       string
	retention time.Duration
	now       func() time.Time

	mu      sync.Mutex
	day     string
	f       *os.File
	size    int64
	dropped int64
}

// Open returns a log writing to dir. retention <= 0 uses DefaultRetention.
// Expired files are removed now and at every day change.
func Open(dir string, retention time.Duration) *Log {
	if retention <= 0 {
		retention = DefaultRetention
	}
	l := &Log{dir: dir, retention: retention, now: time.Now}
	l.prune(l.now())
	return l
}

// SetNow replaces the clock (tests).
func (l *Log) SetNow(now func() time.Time) { l.now = now }

// SetRetention changes the retention (configuration reload).
func (l *Log) SetRetention(d time.Duration) {
	if l == nil {
		return
	}
	if d <= 0 {
		d = DefaultRetention
	}
	l.mu.Lock()
	l.retention = d
	l.mu.Unlock()
}

// Dir returns the directory of the log.
func (l *Log) Dir() string { return l.dir }

// Dropped returns how many events were dropped (write errors, size cap).
func (l *Log) Dropped() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// Append writes ev; Time defaults to now. Errors drop the event: the log
// is for analysis and must never block scheduling.
func (l *Log) Append(ev Event) {
	if l == nil {
		return
	}
	now := l.now()
	if ev.Time.IsZero() {
		ev.Time = now
	}
	ev.Time = ev.Time.UTC()
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	if day != l.day || l.f == nil {
		if l.f != nil {
			l.f.Close()
			l.f = nil
		}
		l.day = day
		l.prune(now)
		if err := os.MkdirAll(l.dir, 0o700); err != nil {
			l.dropped++
			return
		}
		f, err := os.OpenFile(filepath.Join(l.dir, prefix+day+suffix), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			l.dropped++
			return
		}
		fi, _ := f.Stat()
		l.f, l.size = f, 0
		if fi != nil {
			l.size = fi.Size()
		}
	}
	if l.size+int64(len(b)) > MaxFileSize {
		l.dropped++
		return
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		l.dropped++
	}
}

// Close closes the current file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// prune removes daily files older than the retention.
func (l *Log) prune(now time.Time) {
	for _, f := range files(l.dir) {
		if now.Sub(f.day.Add(24*time.Hour)) > l.retention {
			os.Remove(f.path)
		}
	}
}

type dayFile struct {
	path string
	day  time.Time
}

// files lists the daily files of dir, oldest first.
func files(dir string) []dayFile {
	entries, _ := os.ReadDir(dir)
	var out []dayFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		d, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
		if err != nil {
			continue
		}
		out = append(out, dayFile{filepath.Join(dir, name), d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].day.Before(out[j].day) })
	return out
}

// Read returns the events of dir with since <= Time < until (zero bounds
// are open), in file order. Malformed lines are skipped.
func Read(dir string, since, until time.Time) ([]Event, error) {
	var out []Event
	for _, f := range files(dir) {
		if !since.IsZero() && f.day.Add(24*time.Hour).Before(since) {
			continue
		}
		if !until.IsZero() && !f.day.Before(until) {
			continue
		}
		fh, err := os.Open(f.path)
		if err != nil {
			return out, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			if (!since.IsZero() && ev.Time.Before(since)) || (!until.IsZero() && !ev.Time.Before(until)) {
				continue
			}
			out = append(out, ev)
		}
		err = sc.Err()
		fh.Close()
		if err != nil {
			return out, fmt.Errorf("%s: %w", f.path, err)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// ParseSince parses "7d", "36h", "30m" or an RFC 3339 time into the start
// of a window ending at now.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		var n float64
		if _, err := fmt.Sscanf(d, "%g", &n); err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("invalid duration %q", s)
		}
		return now.Add(-time.Duration(n * float64(24*time.Hour))), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid duration %q (e.g. 7d, 36h, 2026-11-01)", s)
	}
	return now.Add(-d), nil
}
