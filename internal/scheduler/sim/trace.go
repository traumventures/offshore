package sim

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/config"
)

// Bytes is a memory amount; JSON accepts a number of bytes or a string
// such as "1.5G".
type Bytes int64

// MarshalJSON writes MiB as a string ("1536M").
func (b Bytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%dM", int64(b)>>20))
}

// UnmarshalJSON implements json.Unmarshaler.
func (b *Bytes) UnmarshalJSON(d []byte) error {
	var n float64
	if err := json.Unmarshal(d, &n); err == nil {
		*b = Bytes(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(d, &s); err != nil {
		return err
	}
	v, err := config.ParseBytes(s)
	*b = Bytes(v)
	return err
}

// Job is one build session of a trace.
type Job struct {
	At         float64 `json:"at"`       // seconds since the trace start
	Repo       string  `json:"repo"`     // build cache affinity
	Env        string  `json:"env"`      // environment hash; machines are per environment
	Duration   float64 `json:"duration"` // seconds with a warm cache and full CPU
	CPU        float64 `json:"cpu"`      // cores used (and requested)
	Memory     Bytes   `json:"memory"`   // configured need (request)
	PeakMemory Bytes   `json:"peak_memory"`
	// SlotBytes is the size of the repository's build slot after a cold
	// build, SlotGrowth what a warm build adds (0: disk not simulated).
	SlotBytes  Bytes `json:"slot_bytes,omitempty"`
	SlotGrowth Bytes `json:"slot_growth,omitempty"`
}

// Trace is a sequence of jobs.
type Trace struct {
	Start time.Time `json:"start"`
	End   float64   `json:"end,omitempty"` // seconds since start the simulation runs at least
	Jobs  []Job     `json:"jobs"`
}

// DefaultStart is used for CSV traces with relative times and no start
// (a Monday, midnight UTC).
var DefaultStart = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

// Load reads a JSON (.json) or CSV trace.
func Load(path string) (Trace, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Trace{}, err
	}
	if strings.EqualFold(filepath.Ext(path), ".csv") {
		return ParseCSV(bytes.NewReader(b))
	}
	var t Trace
	if err := json.Unmarshal(b, &t); err != nil {
		return Trace{}, fmt.Errorf("%s: %w", path, err)
	}
	if t.Start.IsZero() {
		t.Start = DefaultStart
	}
	t.sort()
	return t, nil
}

// Save writes a trace as JSON with one job per line.
func (t Trace) Save(path string) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "{\"start\": %q, \"end\": %g, \"jobs\": [\n", t.Start.Format(time.RFC3339), t.End)
	for i, j := range t.Jobs {
		b, err := json.Marshal(j)
		if err != nil {
			return err
		}
		buf.Write(b)
		if i < len(t.Jobs)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]}\n")
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func (t *Trace) sort() {
	sort.SliceStable(t.Jobs, func(i, j int) bool { return t.Jobs[i].At < t.Jobs[j].At })
}

// ParseCSV reads a trace with the header
// at,repo,env,duration,cpu,memory,peak_memory. at is seconds since the
// start or an RFC 3339 time; a leading "# start=RFC3339" line sets the start.
func ParseCSV(r io.Reader) (Trace, error) {
	br := bufio.NewReader(r)
	t := Trace{}
	var rest bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "#") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(s, "#")), "start="); ok {
				st, perr := time.Parse(time.RFC3339, strings.TrimSpace(v))
				if perr != nil {
					return t, fmt.Errorf("csv start: %w", perr)
				}
				t.Start = st
			}
		} else {
			rest.WriteString(line)
		}
		if err != nil {
			break
		}
	}
	cr := csv.NewReader(&rest)
	cr.TrimLeadingSpace = true
	rows, err := cr.ReadAll()
	if err != nil {
		return t, err
	}
	if len(rows) == 0 {
		return t, fmt.Errorf("csv: empty trace")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, h := range []string{"at", "repo", "env", "duration", "cpu", "memory", "peak_memory"} {
		if _, ok := col[h]; !ok {
			return t, fmt.Errorf("csv: missing column %q", h)
		}
	}
	type abs struct {
		i  int
		at time.Time
	}
	var absolute []abs
	for n, row := range rows[1:] {
		get := func(h string) string { return strings.TrimSpace(row[col[h]]) }
		j := Job{Repo: get("repo"), Env: get("env")}
		fail := func(h string, err error) error { return fmt.Errorf("csv line %d: %s: %w", n+2, h, err) }
		if v, err := strconv.ParseFloat(get("at"), 64); err == nil {
			j.At = v
		} else if at, err := time.Parse(time.RFC3339, get("at")); err == nil {
			absolute = append(absolute, abs{len(t.Jobs), at})
		} else {
			return t, fail("at", err)
		}
		if j.Duration, err = strconv.ParseFloat(get("duration"), 64); err != nil {
			return t, fail("duration", err)
		}
		if j.CPU, err = strconv.ParseFloat(get("cpu"), 64); err != nil {
			return t, fail("cpu", err)
		}
		m, err := config.ParseBytes(get("memory"))
		if err != nil {
			return t, fail("memory", err)
		}
		pk, err := config.ParseBytes(get("peak_memory"))
		if err != nil {
			return t, fail("peak_memory", err)
		}
		j.Memory, j.PeakMemory = Bytes(m), Bytes(pk)
		t.Jobs = append(t.Jobs, j)
	}
	if t.Start.IsZero() {
		t.Start = DefaultStart
		for i, a := range absolute {
			if i == 0 || a.at.Before(t.Start) {
				t.Start = a.at
			}
		}
	}
	for _, a := range absolute {
		t.Jobs[a.i].At = a.at.Sub(t.Start).Seconds()
	}
	t.sort()
	return t, nil
}
