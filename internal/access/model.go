package access

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
)

// Observation is what happened to a slot in one session (or between two
// commits): the trees before and after, and what was read. Writes,
// overwrites and deletes are derived from the trees (what anyfs.Diff of
// Before and After contains).
type Observation struct {
	Before, After *anyfs.Tree
	// Reads are the paths read during the session: the worker's per
	// session read index (atime) or the lower file system's faults.
	// ReadsKnown false: reads were not tracked (noatime), read counts
	// stay out of the classification.
	Reads      []string
	ReadsKnown bool
	// Transient are paths created and deleted again within the session
	// (seen by the overlay or FUSE events, invisible in the trees).
	Transient []string
	At        time.Time
}

// Stats are the access statistics of one pattern, summed over sessions.
type Stats struct {
	// Sessions: sessions with files of the pattern; FileSessions: files
	// present per session, summed (files of the after tree and deleted
	// ones).
	Sessions     int64 `json:"sessions"`
	FileSessions int64 `json:"file_sessions"`
	Creates      int64 `json:"creates,omitempty"`
	// CreateSessions: sessions that created files of the pattern.
	CreateSessions int64 `json:"create_sessions,omitempty"`
	Overwrites     int64 `json:"overwrites,omitempty"`
	Deletes        int64 `json:"deletes,omitempty"`
	Transient      int64 `json:"transient,omitempty"`
	// Reads counts files read; ReadFileSessions the FileSessions of the
	// sessions with known reads (the denominator of the read rate).
	Reads            int64 `json:"reads,omitempty"`
	ReadFileSessions int64 `json:"read_file_sessions,omitempty"`
	// ShortLived counts deletes of files that lived at most one session
	// and were never read; Lived/Lifetime the deletes with a known
	// creation and their summed lifetimes in sessions.
	ShortLived int64 `json:"short_lived,omitempty"`
	Lived      int64 `json:"lived,omitempty"`
	Lifetime   int64 `json:"lifetime,omitempty"`
	// AvgSize is the average file size (bytes, moving average).
	AvgSize int64 `json:"avg_size,omitempty"`
	// Last is the unix time of the last session with files of the pattern.
	Last int64 `json:"last,omitempty"`
}

// Class classifies the statistics (Unknown without observations).
func (s Stats) Class() Class {
	if s.Sessions == 0 {
		return Unknown
	}
	fs := float64(max(s.FileSessions, 1))
	readRate := -1.0
	if s.ReadFileSessions > 0 {
		readRate = float64(s.Reads) / float64(s.ReadFileSessions)
	}
	created := s.Creates + s.Transient
	if created > 0 && float64(s.Transient+s.ShortLived) >= 0.6*float64(created) && readRate < 0.1 {
		return Ephemeral
	}
	if float64(s.Overwrites)/fs >= 0.25 || (s.Deletes > 0 && float64(s.Creates+s.Deletes)/fs >= 0.5) {
		return Churn
	}
	if 2*s.CreateSessions >= s.Sessions && float64(s.Creates)/fs >= 0.05 {
		return WriteOnce
	}
	return Immutable
}

func (s *Stats) add(o Stats, now time.Time) {
	if o.FileSessions > 0 {
		// weight the new session like a tenth of the history at least
		w := max(float64(o.FileSessions)/float64(s.FileSessions+o.FileSessions), 0.1)
		s.AvgSize = int64(float64(s.AvgSize)*(1-w) + float64(o.AvgSize)*w)
	}
	s.Sessions++
	s.FileSessions += o.FileSessions
	s.Creates += o.Creates
	if o.Creates > 0 {
		s.CreateSessions++
	}
	s.Overwrites += o.Overwrites
	s.Deletes += o.Deletes
	s.Transient += o.Transient
	s.Reads += o.Reads
	s.ReadFileSessions += o.ReadFileSessions
	s.ShortLived += o.ShortLived
	s.Lived += o.Lived
	s.Lifetime += o.Lifetime
	s.Last = now.Unix()
}

// Prior is a class hint for the paths matching Glob (Match syntax, e.g.
// "**/target/*/incremental/**"), given by a tool adapter.
type Prior struct {
	Glob  string `json:"glob"`
	Class Class  `json:"class"`
}

// Decision is the class of a path and where it comes from.
type Decision struct {
	Class   Class  `json:"class"`
	Pattern string `json:"pattern"`
	// Source: "learned" (the pattern's statistics), "learned-dir" (the
	// statistics of its directory), "prior" or "default".
	Source   string `json:"source"`
	Sessions int64  `json:"sessions,omitempty"`
}

// Default limits.
const (
	// MinSessions is how many sessions a pattern must be seen in before
	// its statistics override a prior.
	MinSessions = 3
	// MaxPatterns bounds the persisted patterns (least used dropped).
	MaxPatterns = 4096
)

// birth is what the model remembers of a live file (in memory only).
type birth struct {
	seq  int64
	read bool
}

// Model learns access classes per pattern. Safe for concurrent use.
type Model struct {
	mu       sync.Mutex
	seq      int64
	patterns map[string]*Stats // file patterns and directory patterns ("dir/")
	births   map[string]birth
	priors   []Prior
	cache    map[string]Decision
	// MinSessions and MaxPatterns (zero: the defaults).
	MinSessions int
	MaxPatterns int
}

// New returns an empty model with priors.
func New(priors ...Prior) *Model {
	return &Model{patterns: map[string]*Stats{}, births: map[string]birth{}, priors: priors, cache: map[string]Decision{}}
}

// SetPriors replaces the priors (first matching prior wins).
func (m *Model) SetPriors(p []Prior) {
	m.mu.Lock()
	m.priors = append([]Prior(nil), p...)
	m.cache = map[string]Decision{}
	m.mu.Unlock()
}

// Sessions is the number of observed sessions.
func (m *Model) Sessions() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// Stats returns the statistics of a pattern (a directory pattern ends in
// "/").
func (m *Model) Stats(pattern string) (Stats, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.patterns[pattern]
	if !ok {
		return Stats{}, false
	}
	return *s, true
}

// dirKey is the directory pattern of a file pattern.
func dirKey(pat string) string {
	d := path.Dir(pat)
	if d == "." {
		return "/"
	}
	return d + "/"
}

// ObserveLayer observes a session that committed layer on top of before.
func (m *Model) ObserveLayer(before *anyfs.Tree, layer []anyfs.Node, reads []string, readsKnown bool, at time.Time) {
	m.Observe(Observation{Before: before, After: anyfs.Apply(before, layer), Reads: reads, ReadsKnown: readsKnown, At: at})
}

// Observe folds one session into the statistics.
func (m *Model) Observe(o Observation) {
	if o.At.IsZero() {
		o.At = time.Now()
	}
	read := make(map[string]bool, len(o.Reads))
	for _, p := range o.Reads {
		read[p] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	seq := m.seq
	sess := map[string]*Stats{}
	sizes := map[string]int64{}
	at := func(p string) []*Stats {
		pat := Pattern(p)
		out := make([]*Stats, 0, 2)
		for _, k := range []string{pat, dirKey(pat)} {
			s := sess[k]
			if s == nil {
				s = &Stats{}
				sess[k] = s
			}
			out = append(out, s)
		}
		return out
	}
	countRead := func(ss []*Stats, p string) {
		for _, s := range ss {
			if o.ReadsKnown {
				s.ReadFileSessions++
				if read[p] {
					s.Reads++
				}
			}
		}
	}
	for _, n := range o.After.Nodes() {
		if !n.IsFile() {
			continue
		}
		ss := at(n.Path)
		b, ok := o.Before.Get(n.Path)
		created := !ok || !b.IsFile()
		for _, s := range ss {
			s.FileSessions++
			switch {
			case created:
				s.Creates++
			case !b.SameContent(&n):
				s.Overwrites++
			}
		}
		pat := Pattern(n.Path)
		sizes[pat] += n.Size
		sizes[dirKey(pat)] += n.Size
		countRead(ss, n.Path)
		bi, known := m.births[n.Path]
		switch {
		case created:
			m.births[n.Path] = birth{seq: seq, read: read[n.Path]}
		case known && read[n.Path] && !bi.read:
			bi.read = true
			m.births[n.Path] = bi
		}
	}
	for _, n := range o.Before.Nodes() {
		if !n.IsFile() {
			continue
		}
		if a, ok := o.After.Get(n.Path); ok && a.IsFile() {
			continue
		}
		ss := at(n.Path)
		bi, known := m.births[n.Path]
		delete(m.births, n.Path)
		for _, s := range ss {
			s.FileSessions++
			s.Deletes++
			if known {
				life := seq - bi.seq
				s.Lived++
				s.Lifetime += life
				if life <= 1 && o.ReadsKnown && !bi.read && !read[n.Path] {
					s.ShortLived++
				}
			}
		}
		countRead(ss, n.Path)
	}
	for _, p := range o.Transient {
		for _, s := range at(p) {
			s.Transient++
		}
	}
	for k, s := range sess {
		if s.FileSessions > 0 {
			s.AvgSize = sizes[k] / s.FileSessions
		}
		t := m.patterns[k]
		if t == nil {
			t = &Stats{}
			m.patterns[k] = t
		}
		t.add(*s, o.At)
	}
	m.trim()
	m.cache = map[string]Decision{}
}

// trim drops the least used patterns above MaxPatterns.
func (m *Model) trim() {
	limit := m.MaxPatterns
	if limit <= 0 {
		limit = MaxPatterns
	}
	if len(m.patterns) <= limit {
		return
	}
	keys := make([]string, 0, len(m.patterns))
	for k := range m.patterns {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := m.patterns[keys[i]], m.patterns[keys[j]]
		if a.Last != b.Last {
			return a.Last < b.Last
		}
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys[:len(keys)-limit] {
		delete(m.patterns, k)
	}
}

// Classify returns the class of a path: the pattern's statistics once
// seen in MinSessions sessions, else its directory's, else the first
// matching prior, else whatever the statistics say (Unknown without).
func (m *Model) Classify(p string) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.cache[p]; ok {
		return d
	}
	d := m.classify(p)
	if len(m.cache) > 1<<16 {
		m.cache = map[string]Decision{}
	}
	m.cache[p] = d
	return d
}

func (m *Model) classify(p string) Decision {
	pat := Pattern(p)
	minS := int64(m.MinSessions)
	if minS <= 0 {
		minS = MinSessions
	}
	s := m.patterns[pat]
	if s != nil && s.Sessions >= minS {
		return Decision{Class: s.Class(), Pattern: pat, Source: "learned", Sessions: s.Sessions}
	}
	if ds := m.patterns[dirKey(pat)]; ds != nil && ds.Sessions >= minS {
		return Decision{Class: ds.Class(), Pattern: pat, Source: "learned-dir", Sessions: ds.Sessions}
	}
	for _, pr := range m.priors {
		if Match(pr.Glob, p) {
			return Decision{Class: pr.Class, Pattern: pat, Source: "prior"}
		}
	}
	if s != nil {
		return Decision{Class: s.Class(), Pattern: pat, Source: "learned", Sessions: s.Sessions}
	}
	return Decision{Class: Unknown, Pattern: pat, Source: "default"}
}

// Policy returns the cache policy of a path (path -> class -> policy).
func (m *Model) Policy(p string) Policy { return PolicyFor(m.Classify(p).Class) }

// file is the persisted form of a model.
type file struct {
	Version  int               `json:"version"`
	Sessions int64             `json:"sessions"`
	Patterns map[string]*Stats `json:"patterns"`
}

const fileVersion = 1

// MarshalJSON persists the statistics (not the priors nor live files).
func (m *Model) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Marshal(file{Version: fileVersion, Sessions: m.seq, Patterns: m.patterns})
}

// UnmarshalJSON loads persisted statistics (other versions are ignored).
func (m *Model) UnmarshalJSON(b []byte) error {
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.births == nil {
		m.births = map[string]birth{}
	}
	m.cache = map[string]Decision{}
	m.patterns = map[string]*Stats{}
	m.seq = 0
	if f.Version != fileVersion {
		return nil
	}
	for k, s := range f.Patterns {
		if s != nil {
			m.patterns[k] = s
		}
	}
	m.seq = f.Sessions
	return nil
}

// Load reads a model saved by Save; a missing or unreadable file gives an
// empty model. priors seed it.
func Load(file string, priors ...Prior) *Model {
	m := New(priors...)
	if b, err := os.ReadFile(file); err == nil {
		if json.Unmarshal(b, m) != nil {
			m = New(priors...)
		}
	}
	return m
}

// Save writes the model atomically.
func (m *Model) Save(file string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
