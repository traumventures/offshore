// Package runlog keeps a bounded history of proxied runs for
// `cloudsourced why`: where a command ran and why, on which machine, how
// long each phase took and what it consumed.
package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Keep is the number of runs retained.
const Keep = 50

// FileName is the log file inside the state directory.
const FileName = "runs.jsonl"

// Modes of a run.
const (
	Local  = "local"
	Remote = "remote"
	Failed = "failed" // remote required but not possible
)

// Run is one proxied command.
type Run struct {
	Time  time.Time `json:"time"`
	Repo  string    `json:"repo,omitempty"`
	Cwd   string    `json:"cwd,omitempty"`
	Tool  string    `json:"tool"`
	Args  []string  `json:"args,omitempty"`
	Mode  string    `json:"mode"`            // local | remote | failed
	Why   string    `json:"why"`             // decision reason
	Error string    `json:"error,omitempty"` // failure after the remote start
	Exit  *int      `json:"exit,omitempty"`  // unknown for local runs (exec)
	Ports []string  `json:"ports,omitempty"` // forwarded ports local:remote
	Lease string    `json:"lease,omitempty"` // fleet lease id
	Slot  string    `json:"slot,omitempty"`  // build slot on the machine
	Warm  bool      `json:"warm,omitempty"`  // build slot was warm

	Machine   string  `json:"machine,omitempty"`
	Provider  string  `json:"provider,omitempty"`
	Size      string  `json:"size,omitempty"`
	Price     float64 `json:"price_per_hour,omitempty"`
	Placement string  `json:"placement,omitempty"` // placement reason of the policy

	Timings Timings          `json:"timings"`
	Usage   *scheduler.Usage `json:"usage,omitempty"`
	// Cache holds counters reported by cache integrations, e.g.
	// "sccache_hits" and "sccache_misses".
	Cache map[string]int64 `json:"cache,omitempty"`
}

// Timings are phase durations in milliseconds.
type Timings struct {
	Env       int64 `json:"env_ms"`       // environment image (build or check)
	Sync      int64 `json:"sync_ms"`      // local manifest refresh
	Queue     int64 `json:"queue_ms"`     // waiting for capacity (incl. machine start)
	Attach    int64 `json:"attach_ms"`    // projecting roots onto the machine
	Connect   int64 `json:"connect_ms"`   // proxy connection to the machine
	Barrier   int64 `json:"barrier_ms"`   // server side preparation until start
	Run       int64 `json:"run_ms"`       // the process itself
	Writeback int64 `json:"writeback_ms"` // source changes copied back
	Outputs   int64 `json:"outputs_ms"`   // output transfer
	Total     int64 `json:"total_ms"`
}

// Ms converts a duration to milliseconds.
func Ms(d time.Duration) int64 { return d.Milliseconds() }

// Path returns the log file in dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Append adds a run to the log in dir and trims it to the last Keep runs.
// Writers are serialized with a lock file; the file is rewritten only
// when it holds twice the retained number of runs.
func Append(dir string, r Run) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(Path(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	lines, err := readLines(Path(dir))
	if err != nil || len(lines) <= 2*Keep {
		return err
	}
	keep := lines[len(lines)-Keep:]
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(bytes.Join(keep, []byte("\n")), '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

// Recent returns up to n runs, most recent first (n <= 0: all retained).
func Recent(dir string, n int) ([]Run, error) {
	lines, err := readLines(Path(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(lines) > Keep {
		lines = lines[len(lines)-Keep:]
	}
	var out []Run
	for i := len(lines) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		var r Run
		if json.Unmarshal(lines[i], &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func readLines(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
			lines = append(lines, append([]byte(nil), l...))
		}
	}
	return lines, sc.Err()
}

func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, FileName+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
