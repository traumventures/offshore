// Package events is the orchestrator's analysis event log: one JSON object
// per line in daily files (events-YYYY-MM-DD.jsonl, UTC) in the state
// directory, removed after a retention period. It is always on and
// independent of tracing; `offshore report` and `offshore sim
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
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/traumventures/offshore/internal/scheduler"
)

// Event types.
const (
	// TaskPlaced: a request was decided (Placement: assign, reject,
	// timeout, canceled, error), with arrival time and queue wait.
	TaskPlaced = "task_placed"
	// TaskEnd: a session finished and its usage was reported, with
	// Predicted (what the scheduler expected) next to Usage.
	TaskEnd = "task_end"
	// TaskWaiting: a queued request is still waiting (every
	// WaitingInterval): what for (Waiting) and the reason.
	TaskWaiting = "task_waiting"
	// TaskKilled: a session was killed by the machine rather than ending
	// on its own (Usage.Killer: cgroup_oom, machine_oom, worker_shutdown;
	// Usage.OOM for memory), written next to its task_end so OOM kills
	// and machine stops under running sessions are easy to find.
	TaskKilled = "task_killed"
	// SessionSample: a running session's cgroup, sampled by the worker
	// every 5 s (Sample; Task, Machine, Repo, Key, Labels).
	SessionSample = "session_sample"

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

	// SnapshotSave and SnapshotRestore: a build slot was committed and
	// flushed to an object store (its head) or a head was mounted on a
	// machine (Repo, Slot, Bytes, Pages written, Seconds, Store, Provider,
	// Machine).
	SnapshotSave    = "snapshot_save"
	SnapshotRestore = "snapshot_restore"
	// SlotAction: the scheduler handled a full or growing build slot
	// (Reason: resize, prune, move, cold; Repo, Machine, Bytes).
	SlotAction = "slot_action"
	// Volume events (Volume, Provider, Zone, Machine, Bytes, Seconds):
	// a cache volume was created, attached to a new machine, detached
	// from a machine that is gone (kept for reuse, or followed by
	// VolumeDelete), grown (Bytes: the new size) or deleted. Cost is the
	// volume's storage cost since its previous event.
	VolumeCreate = "volume_create"
	VolumeAttach = "volume_attach"
	VolumeDetach = "volume_detach"
	VolumeResize = "volume_resize"
	VolumeDelete = "volume_delete"
	// Cache file system (anyfs) events in virtual memory terms, reported
	// per machine (Machine, Repo, Slot, Pages, Bytes, Seconds): a page
	// fault (content not resident, fetched from a lower tier), page-in
	// and page-out (pages moved between tiers), writeback (dirty pages,
	// e.g. a committed layer, written to the store), reclaim (resident
	// pages dropped) and thrash (working set larger than the local
	// capacity: the scheduler should pick a bigger disk or machine).
	PageFault = "page_fault"
	PageIn    = "page_in"
	PageOut   = "page_out"
	Writeback = "writeback"
	Reclaim   = "reclaim"
	Thrash    = "thrash"
	// ProviderDown: a create or start failed with a provider error of
	// ErrorClass. Reason "cooldown" (capacity, quota): no new machines for
	// Seconds (backoff); "disabled" (account_limit, auth): none until the
	// configuration changes or `offshore provider reset`.
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
	// Decision lists the options the policy considered (task_placed,
	// machine_create, machine_start).
	Decision *scheduler.Decision `json:"decision,omitempty"`
	// Estimate is where Need came from (task_placed, task_end).
	Estimate *scheduler.Estimate `json:"estimate,omitempty"`
	// Predicted is what the scheduler expected of the run (task_end).
	Predicted *Predicted `json:"predicted,omitempty"`
	// Waiting is what a queued task waits for (task_waiting).
	Waiting *Waiting `json:"waiting,omitempty"`
	// Sample is one sample of a session (session_sample).
	Sample *Sample `json:"sample,omitempty"`

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
	// ErrorClass classifies a provider error (ProviderDown, a failed
	// create): capacity, quota, account_limit, auth, transient, other.
	ErrorClass string `json:"error_class,omitempty"`

	// MachineSample fields.
	Reserved *scheduler.Resources `json:"reserved,omitempty"` // sum of session reservations
	Used     *scheduler.Resources `json:"used,omitempty"`     // busy cores, memory of sessions
	Pressure *scheduler.Pressure  `json:"pressure,omitempty"` // PSI some avg10, 0..1
	Sessions int                  `json:"sessions,omitempty"`
	Disk     *Disk                `json:"disk,omitempty"` // machine disk usage

	// Snapshot fields (SnapshotSave, SnapshotRestore, SlotAction).
	Slot    string  `json:"slot,omitempty"`
	Store   string  `json:"store,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// Pages is the page count of a cache file system event.
	Pages int64 `json:"pages,omitempty"`
	// VM is the cache file system state of a machine (MachineSample,
	// Thrash).
	VM *scheduler.VM `json:"vm,omitempty"`

	// Volume fields.
	Volume string  `json:"volume,omitempty"`
	Zone   string  `json:"zone,omitempty"`
	Cost   float64 `json:"cost,omitempty"`

	// Labels describe who ran a task (worktree, repo, branch, user, agent
	// and free labels; see internal/labels). Set on task events.
	Labels map[string]string `json:"labels,omitempty"`

	// Boot is the phase breakdown of a machine's create or start
	// (MachineReady).
	Boot []BootPhase `json:"boot,omitempty"`

	// TraceID correlates the event with a distributed trace.
	TraceID string `json:"trace_id,omitempty"`
}

// BootPhase is one step of a machine's create or start, reached Ms
// milliseconds after it was requested: "provider" (the provider call
// returned: instance running), the machine's own phases (kernel,
// user_data, docker, binary, supervisor, docker_ready, cache,
// registry_login, image, secrets, containers, worker, listening, ready;
// host clock) and "observed" (the orchestrator saw it ready). Bytes is
// a size of the step (the pulled image).
type BootPhase struct {
	Phase string `json:"phase"`
	Ms    int64  `json:"ms"`
	Bytes int64  `json:"bytes,omitempty"`
}

// WaitingInterval is how often a queued task emits task_waiting.
const WaitingInterval = 15 * time.Second

// Predicted is what the scheduler expected of a run when it placed it:
// the run time of the chosen option (median and pessimistic end), the
// mean busy cores and the peak memory of the estimate.
type Predicted struct {
	RunSeconds      float64 `json:"run_seconds,omitempty"`
	ExpectedSeconds float64 `json:"expected_seconds,omitempty"`
	Cores           float64 `json:"cores,omitempty"`
	PeakMemory      int64   `json:"peak_memory,omitempty"`
	// Actual values of the same quantities (from Usage) and the ratios
	// actual / predicted (0: unknown).
	ActualRunSeconds float64 `json:"actual_run_seconds,omitempty"`
	ActualCores      float64 `json:"actual_cores,omitempty"`
	ActualPeakMemory int64   `json:"actual_peak_memory,omitempty"`
	RunRatio         float64 `json:"run_ratio,omitempty"`
	CoresRatio       float64 `json:"cores_ratio,omitempty"`
	MemoryRatio      float64 `json:"memory_ratio,omitempty"`
}

// Compare fills in the actual values from u and the ratios.
func (p *Predicted) Compare(u scheduler.Usage) {
	p.ActualRunSeconds = round1(u.Wall.Seconds())
	if u.Wall > 0 {
		p.ActualCores = round2(u.CPUSeconds / u.Wall.Seconds())
	}
	p.ActualPeakMemory = u.PeakMemory
	ratio := func(a, b float64) float64 {
		if a <= 0 || b <= 0 {
			return 0
		}
		return round2(a / b)
	}
	p.RunRatio = ratio(p.ActualRunSeconds, p.RunSeconds)
	p.CoresRatio = ratio(p.ActualCores, p.Cores)
	p.MemoryRatio = ratio(float64(p.ActualPeakMemory), float64(p.PeakMemory))
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// Waiting is what a queued task waits for: For is wait_machine (a busy
// or starting Machine), wait_capacity (machines being created or started
// for the environment) or none (nothing on its way yet). ETASeconds is
// the expected time until it can start (0: unknown).
type Waiting struct {
	For        string  `json:"for"`
	Machine    string  `json:"machine,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Size       string  `json:"size,omitempty"`
	Creating   int     `json:"creating,omitempty"` // machines of the environment on their way
	ETASeconds float64 `json:"eta_seconds,omitempty"`
}

// Sample is one sample of a running session's cgroup: busy cores since
// the previous sample, current memory, cumulative IO bytes and PSI "some
// avg10" (0..1) of cpu, memory and io.
type Sample struct {
	Time       time.Time `json:"time"`
	CPU        float64   `json:"cpu"`
	Memory     int64     `json:"memory"`
	IORead     int64     `json:"io_read_bytes,omitempty"`
	IOWrite    int64     `json:"io_write_bytes,omitempty"`
	PSICPU     float64   `json:"psi_cpu,omitempty"`
	PSIMemory  float64   `json:"psi_memory,omitempty"`
	PSIIO      float64   `json:"psi_io,omitempty"`
	MemoryHigh int64     `json:"memory_high,omitempty"` // memory.high events so far (throttling)
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
//
// Every event is one complete line written with a single write(2) on an
// O_APPEND file. A failed or short write (ENOSPC, EIO) is undone by
// truncating the file back to its size before the write, so readers never
// see a partial line; the event is counted in Dropped and the first
// failure is logged (warn).
type Log struct {
	dir       string
	retention time.Duration
	now       func() time.Time

	taps []func(Event) // see Tap
	// warn reports the first dropped event (nil: log.Printf).
	warn func(msg string, err error)

	mu      sync.Mutex
	day     string
	f       *os.File
	size    int64
	dropped int64
	warned  bool
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

// SetWarn sets how the first dropped event is reported (before concurrent
// use).
func (l *Log) SetWarn(fn func(msg string, err error)) {
	if l != nil {
		l.warn = fn
	}
}

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

// Tap calls fn with every event appended from now on (before it is
// written, Time set; also when writing fails). Taps are registered before
// the log is used concurrently and must not block.
func (l *Log) Tap(fn func(Event)) {
	if l != nil {
		l.taps = append(l.taps, fn)
	}
}

// Dir returns the directory of the log.
func (l *Log) Dir() string { return l.dir }

// Dropped returns how many events were dropped (write errors, size cap).
func (l *Log) Dropped() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// dropLocked counts a dropped event and reports the first one.
func (l *Log) dropLocked(why string, err error) {
	l.dropped++
	if l.warned {
		return
	}
	l.warned = true
	msg := "event log: dropping events (" + why + "); counted in offshore_events_dropped_total"
	if l.warn != nil {
		l.warn(msg, err)
	} else {
		log.Printf("%s: %v", msg, err)
	}
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
	for _, fn := range l.taps {
		fn(ev)
	}
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
			l.dropLocked("create directory", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(l.dir, prefix+day+suffix), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			l.dropLocked("open file", err)
			return
		}
		fi, _ := f.Stat()
		l.f, l.size = f, 0
		if fi != nil {
			l.size = fi.Size()
		}
	}
	if l.size+int64(len(b)) > MaxFileSize {
		l.dropLocked("daily file size cap", fmt.Errorf("%d bytes", MaxFileSize))
		return
	}
	if err := writeLine(l.f, b, l.size); err != nil {
		l.dropLocked("write failed", err)
		// The file may have a different size now (another writer): resync.
		if fi, serr := l.f.Stat(); serr == nil {
			l.size = fi.Size()
		}
		return
	}
	l.size += int64(len(b))
}

// lineWriter is the part of *os.File writeLine uses (tests fake it).
type lineWriter interface {
	Write([]byte) (int, error)
	Truncate(size int64) error
}

// writeLine writes b (one complete line) with a single write and undoes a
// short or failed write by truncating to before (the size before the
// write), so the file never holds a partial line.
func writeLine(f lineWriter, b []byte, before int64) error {
	n, err := f.Write(b)
	if err == nil && n == len(b) {
		return nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	if n > 0 {
		if terr := f.Truncate(before); terr != nil {
			return fmt.Errorf("%w (truncating the partial line: %v)", err, terr)
		}
	}
	return err
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
// are open), in file order. Unparsable lines are skipped and counted (see
// SkippedLines).
func Read(dir string, since, until time.Time) ([]Event, error) {
	evs, _, err := ReadCounted(dir, since, until)
	return evs, err
}

// skippedLines counts unparsable lines skipped by all readers of this
// process (offshore_events_skipped_lines_total).
var skippedLines atomic.Int64

// SkippedLines returns how many unparsable lines readers skipped.
func SkippedLines() int64 { return skippedLines.Load() }

// ReadCounted is Read that also returns the number of unparsable lines it
// skipped.
func ReadCounted(dir string, since, until time.Time) ([]Event, int, error) {
	var out []Event
	skipped := 0
	defer func() { skippedLines.Add(int64(skipped)) }()
	for _, f := range files(dir) {
		if !since.IsZero() && f.day.Add(24*time.Hour).Before(since) {
			continue
		}
		if !until.IsZero() && !f.day.Before(until) {
			continue
		}
		fh, err := os.Open(f.path)
		if err != nil {
			return out, skipped, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			var ev Event
			if json.Unmarshal(line, &ev) != nil {
				skipped++
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
			return out, skipped, fmt.Errorf("%s: %w", f.path, err)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, skipped, nil
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
