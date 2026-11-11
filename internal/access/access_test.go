package access

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
)

func TestPattern(t *testing.T) {
	cases := map[string]string{
		"target/debug/deps/libserde-0123456789abcdef.rlib":                                      "target/debug/deps/*.rlib",
		"target/debug/deps/serde-0123456789abcdef.d":                                            "target/debug/deps/*.d",
		"target/debug/incremental/serde-1x2y3z4w5v6u/s-h0b1e3z4ld-1a2b3c-working/dep-graph.bin": "target/debug/incremental/*/s-*/dep-graph.bin",
		"target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json":                       "target/debug/.fingerprint/*/*.json",
		"target/debug/.fingerprint/serde-0123456789abcdef/invoked.timestamp":                    "target/debug/.fingerprint/*/invoked.timestamp",
		"target/debug/build/ring-0123456789abcdef/out/libring.a":                                "target/debug/build/*/out/*.a",
		"target/tmp/rustcAb12Cd34/x.o":                                                          "target/tmp/*/x.o",
		"node_modules/.cache/12345/index.json":                                                  "node_modules/.cache/*/index.json",
		"src/main.rs":                                                                           "src/main.rs",
		"target/x86_64-unknown-linux-gnu/release/app":                                           "target/x86_64-unknown-linux-gnu/release/app",
		"go/pkg/mod/cache/v1.2.3/a.zip":                                                         "go/pkg/mod/cache/v1.2.3/a.zip",
	}
	for in, want := range cases {
		if got := Pattern(in); got != want {
			t.Errorf("Pattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		glob, p string
		ok      bool
	}{
		{"target/*/deps/**", "target/debug/deps/libx.rlib", true},
		{"target/*/deps/**", "target/debug/build/x", false},
		{"**/target/*/incremental/**", "crates/a/target/debug/incremental/x/s-1/f", true},
		{"**/target/*/incremental/**", "target/debug/incremental/x", true},
		{"**/node_modules/**", "node_modules", true},
		{"src/*.rs", "src/a/b.rs", false},
	}
	for _, c := range cases {
		if got := Match(c.glob, c.p); got != c.ok {
			t.Errorf("Match(%q, %q) = %v", c.glob, c.p, got)
		}
	}
}

func fnode(p string, size int64, digest string) anyfs.Node {
	return anyfs.Node{Path: p, Kind: anyfs.File, Mode: 0o644, Size: size, Digest: digest}
}

// cargoSession builds the tree after cargo session i (1-based) and what
// it read and created transiently.
func cargoSession(i int) (nodes []anyfs.Node, reads []string, transient []string) {
	for c := range 5 {
		p := fmt.Sprintf("target/debug/deps/libcrate%d-%016x.rlib", c, 0x1000+c)
		nodes = append(nodes, fnode(p, 1<<20, fmt.Sprintf("dep%d", c)))
		reads = append(reads, p)
	}
	for k := 1; k <= i; k++ { // one new build script output per session, kept
		nodes = append(nodes, fnode(fmt.Sprintf("target/debug/build/gen%d-%016x/out/gen.rs", k, 0x2000+k), 4096, fmt.Sprint("gen", k)))
	}
	// incremental: a new session directory, the previous one (read) gone
	for f := range 3 {
		nodes = append(nodes, fnode(fmt.Sprintf("target/debug/incremental/app-%016x/s-hq%04dzz-working/part%d.bin", 0x3000, i, f), 8192, fmt.Sprint("inc", i, f)))
		if i > 1 {
			reads = append(reads, fmt.Sprintf("target/debug/incremental/app-%016x/s-hq%04dzz-working/part%d.bin", 0x3000, i-1, f))
		}
	}
	// fingerprint overwritten every session
	nodes = append(nodes, fnode(fmt.Sprintf("target/debug/.fingerprint/app-%016x/bin-app", 0x4000), 16, fmt.Sprint("fp", i)))
	// tmp: created, deleted next session, never read
	nodes = append(nodes, fnode(fmt.Sprintf("target/tmp/rustc%02dAb%02dCd/x.o", i, i), 100, fmt.Sprint("tmp", i)))
	transient = []string{fmt.Sprintf("target/debug/deps/rmeta%04dxyz/out.tmp", i)}
	nodes = append(nodes, fnode("src/main.rs", 10, "main"))
	reads = append(reads, "src/main.rs")
	return nodes, reads, transient
}

func runCargo(m *Model, sessions int) { runCargoFrom(m, anyfs.NewTree(nil), 1, sessions) }

func runCargoFrom(m *Model, before *anyfs.Tree, first, last int) *anyfs.Tree {
	start := time.Unix(1_700_000_000, 0)
	for i := first; i <= last; i++ {
		nodes, reads, transient := cargoSession(i)
		after := anyfs.NewTree(nodes)
		m.Observe(Observation{Before: before, After: after, Reads: reads, ReadsKnown: true, Transient: transient, At: start.Add(time.Duration(i) * time.Hour)})
		before = after
	}
	return before
}

func TestModelLearnsCargoClasses(t *testing.T) {
	m := New()
	runCargo(m, 6)
	want := map[string]Class{
		"target/debug/deps/libcrate1-0000000000001001.rlib":                          Immutable,
		"target/debug/build/gen9-0000000000002009/out/gen.rs":                        WriteOnce,
		"target/debug/incremental/app-0000000000003000/s-hq0099zz-working/part0.bin": Churn,
		"target/debug/.fingerprint/app-0000000000004000/bin-app":                     Churn,
		"target/tmp/rustc99Ab99Cd/x.o":                                               Ephemeral,
		"src/main.rs":                                                                Immutable,
	}
	for p, c := range want {
		d := m.Classify(p)
		if d.Class != c || d.Source != "learned" {
			st, _ := m.Stats(d.Pattern)
			t.Errorf("%s: %+v want %v (stats %+v)", p, d, c, st)
		}
	}
	if st, _ := m.Stats("target/debug/deps/*.rlib"); st.AvgSize != 1<<20 || st.Sessions != 6 {
		t.Errorf("deps stats %+v", st)
	}
	// transient files under deps are ephemeral by their own pattern
	if d := m.Classify("target/debug/deps/rmeta0042xyz/out.tmp"); d.Class != Ephemeral {
		t.Errorf("transient: %+v", d)
	}
}

func TestPriorsUntilLearned(t *testing.T) {
	priors := []Prior{{Glob: "**/target/*/deps/**", Class: Churn}, {Glob: "**/target/*/.fingerprint/**", Class: Churn}}
	m := New(priors...)
	p := "target/debug/deps/libcrate1-0000000000001001.rlib"
	if d := m.Classify(p); d.Class != Churn || d.Source != "prior" {
		t.Fatalf("before learning: %+v", d)
	}
	tree := runCargoFrom(m, anyfs.NewTree(nil), 1, 2)
	if d := m.Classify(p); d.Source != "prior" {
		t.Fatalf("2 sessions must not override the prior: %+v", d)
	}
	runCargoFrom(m, tree, 3, 3)
	if d := m.Classify(p); d.Class != Immutable || d.Source != "learned" {
		t.Fatalf("learned must override: %+v", d)
	}
	if got := m.Policy(p); !got.Prefetch || !got.Dedupe || got.LocalOnly {
		t.Errorf("immutable policy %+v", got)
	}
	// an unseen file of a known directory uses the directory's statistics
	if d := m.Classify("target/debug/deps/libother.so"); d.Source != "learned-dir" || d.Class != Immutable {
		t.Errorf("dir fallback: %+v", d)
	}
	if d := m.Classify("README.md"); d.Class != Unknown || d.Source != "default" {
		t.Errorf("unknown: %+v", d)
	}
}

func TestSaveLoad(t *testing.T) {
	m := New()
	runCargo(m, 4)
	f := filepath.Join(t.TempDir(), "x", "access.json")
	if err := m.Save(f); err != nil {
		t.Fatal(err)
	}
	l := Load(f, Prior{Glob: "README.md", Class: Immutable})
	if l.Sessions() != 4 {
		t.Fatalf("sessions %d", l.Sessions())
	}
	p := "target/debug/.fingerprint/app-0000000000004000/bin-app"
	if a, b := m.Classify(p), l.Classify(p); a != b {
		t.Errorf("%+v != %+v", a, b)
	}
	if d := l.Classify("README.md"); d.Source != "prior" {
		t.Errorf("priors after load: %+v", d)
	}
	if e := Load(filepath.Join(t.TempDir(), "missing.json")); e.Sessions() != 0 {
		t.Error("missing file")
	}
	var other Model
	if err := json.Unmarshal([]byte(`{"version":99,"sessions":5,"patterns":{"a":{"sessions":1}}}`), &other); err != nil || other.Sessions() != 0 {
		t.Errorf("foreign version must be ignored: %v %d", err, other.Sessions())
	}
}

func TestTrim(t *testing.T) {
	m := New()
	m.MaxPatterns = 4
	var nodes []anyfs.Node
	for i := range 10 {
		nodes = append(nodes, fnode(fmt.Sprintf("d%d/f", i), 1, "x"))
	}
	m.Observe(Observation{Before: anyfs.NewTree(nil), After: anyfs.NewTree(nodes)})
	if n := len(m.patterns); n != 4 {
		t.Fatalf("patterns %d", n)
	}
}

func TestObserveLayer(t *testing.T) {
	m := New()
	base := anyfs.NewTree([]anyfs.Node{fnode("a", 1, "1"), fnode("b", 1, "1")})
	top := anyfs.NewTree([]anyfs.Node{fnode("a", 2, "2"), fnode("c", 1, "1")})
	m.ObserveLayer(base, anyfs.Diff(base, top), nil, false, time.Time{})
	st, _ := m.Stats("/")
	if st.Creates != 1 || st.Overwrites != 1 || st.Deletes != 1 || st.FileSessions != 3 || st.ReadFileSessions != 0 {
		t.Errorf("root dir stats %+v", st)
	}
}

func TestPolicyFor(t *testing.T) {
	if p := PolicyFor(Churn); !p.LocalOnly || p.WritebackDelay != ChurnWritebackDelay || !p.SkipDeleted {
		t.Errorf("churn %+v", p)
	}
	if p := PolicyFor(Ephemeral); !p.NoWriteback || p.ReclaimOrder != 0 {
		t.Errorf("ephemeral %+v", p)
	}
	order := []Class{Ephemeral, Churn, Unknown, Immutable}
	for i := 1; i < len(order); i++ {
		if PolicyFor(order[i-1]).ReclaimOrder >= PolicyFor(order[i]).ReclaimOrder {
			t.Errorf("reclaim order %v before %v", order[i-1], order[i])
		}
	}
	for c := Unknown; c <= Ephemeral; c++ {
		var back Class
		b, _ := c.MarshalText()
		if err := back.UnmarshalText(b); err != nil || back != c {
			t.Errorf("roundtrip %v", c)
		}
	}
}
