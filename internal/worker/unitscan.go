package worker

import (
	"io"
	"regexp"
	"strings"
	"sync"
)

// unitScanner watches a session's output for the build units the tool
// reports (cargo: "   Compiling foo v1.0 (...)", "Checking", "Documenting")
// and cargo's fingerprint log ("dirty: Reason(...)" with CARGO_LOG
// enabling cargo::core::compiler::fingerprint=info), so task_end can show
// whether a "warm" session rebuilt and why. It only reads a copy of the
// output; lines are split on \n and \r (progress bars).
type unitScanner struct {
	mu      sync.Mutex
	line    []byte
	units   int
	sample  []string
	reasons map[string]int
}

const (
	unitSampleMax = 8
	unitLineMax   = 4096
)

var (
	ansiRE  = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	unitRE  = regexp.MustCompile(`^\s*(Compiling|Checking|Documenting) (\S+)`)
	dirtyRE = regexp.MustCompile(`fingerprint: dirty: ([A-Za-z]+)(?:\(([A-Za-z]+))?`)
)

// Write implements io.Writer.
func (u *unitScanner) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			u.scanLocked()
			continue
		}
		if len(u.line) < unitLineMax {
			u.line = append(u.line, b)
		}
	}
	return len(p), nil
}

func (u *unitScanner) scanLocked() {
	if len(u.line) == 0 {
		return
	}
	s := string(u.line)
	u.line = u.line[:0]
	if strings.Contains(s, "\x1b") {
		s = ansiRE.ReplaceAllString(s, "")
	}
	if m := unitRE.FindStringSubmatch(s); m != nil {
		u.units++
		if len(u.sample) < unitSampleMax {
			u.sample = append(u.sample, m[2])
		}
		return
	}
	if m := dirtyRE.FindStringSubmatch(s); m != nil {
		r := m[1]
		if m[2] != "" {
			r += "/" + m[2]
		}
		if u.reasons == nil {
			u.reasons = map[string]int{}
		}
		u.reasons[r]++
	}
}

// result returns the units built, a sample of them and the dirty reasons.
func (u *unitScanner) result() (int, []string, map[string]int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.scanLocked()
	return u.units, append([]string(nil), u.sample...), u.reasons
}

// teeWriter writes to w and copies to the scanner.
type teeWriter struct {
	w io.Writer
	u *unitScanner
}

func (t teeWriter) Write(p []byte) (int, error) {
	t.u.Write(p)
	return t.w.Write(p)
}
