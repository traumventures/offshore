package adapter

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
)

func split(s string) []string { return strings.Fields(s) }

func TestParseCargo(t *testing.T) {
	c := parseCargo(split("+nightly --color always -Z unstable-options --locked nextest run -p x"))
	if c.Sub != "nextest" || !reflect.DeepEqual(c.Global, split("+nightly --color always -Z unstable-options --locked")) || !reflect.DeepEqual(c.Rest, split("run -p x")) {
		t.Fatalf("%+v", c)
	}
	if c := parseCargo(split("-C sub test")); c.Sub != "test" {
		t.Fatalf("%+v", c)
	}
}

func TestParseCargoTest(t *testing.T) {
	cases := []struct {
		args               string
		build, binary      string
		doc, quiet, nofail bool
		unsplit            bool
		docArgs            string
	}{
		{args: "", doc: true, docArgs: "test --doc"},
		{args: "foo", binary: "foo", doc: true, docArgs: "test --doc foo"},
		{args: "-p app-store foo", build: "-p app-store", binary: "foo", doc: true, docArgs: "test --doc -p app-store foo"},
		{args: "-papp-store --package=b foo -- --nocapture --test-threads 1", build: "-papp-store --package=b", binary: "foo --nocapture --test-threads 1", doc: true,
			docArgs: "test --doc -papp-store --package=b foo -- --nocapture --test-threads 1"},
		{args: "--workspace -F a,b --features c --release --profile ci --target-dir t -j4 -- --ignored", build: "--workspace -F a,b --features c --release --profile ci --target-dir t -j4", binary: "--ignored", doc: true,
			docArgs: "test --doc --workspace -F a,b --features c --release --profile ci --target-dir t -j4 -- --ignored"},
		{args: "--lib foo", build: "--lib", binary: "foo"},
		{args: "--test it -p a", build: "--test it -p a"},
		{args: "--all-targets", build: "--all-targets"},
		{args: "-q --no-fail-fast x", build: "-q --no-fail-fast", binary: "--quiet x", doc: true, quiet: true, nofail: true},
		{args: "-qv", build: "-qv", binary: "--quiet", doc: true, quiet: true},
		{args: "--timings --timings=html", build: "--timings --timings=html", doc: true},
		{args: "-- foo -p x", binary: "foo -p x", doc: true},
		{args: "--no-run", unsplit: true},
		{args: "--doc", unsplit: true},
		{args: "--message-format json", unsplit: true},
		{args: "--bogus", unsplit: true},
		{args: "a b", unsplit: true},
		{args: "--help", unsplit: true},
	}
	for _, c := range cases {
		got := parseCargoTest(nil, split(c.args))
		if (got.Unsplit != "") != c.unsplit {
			t.Errorf("%q: unsplit=%q", c.args, got.Unsplit)
			continue
		}
		if c.unsplit {
			continue
		}
		if !reflect.DeepEqual(got.Build, split(c.build)) && !(len(got.Build) == 0 && c.build == "") {
			t.Errorf("%q: build %q", c.args, got.Build)
		}
		if b := got.binaryArgs(); !reflect.DeepEqual(b, split(c.binary)) && !(len(b) == 0 && c.binary == "") {
			t.Errorf("%q: binary args %q", c.args, b)
		}
		if got.DocTests != c.doc || got.Quiet != c.quiet || got.NoFail != c.nofail {
			t.Errorf("%q: doc=%v quiet=%v nofail=%v", c.args, got.DocTests, got.Quiet, got.NoFail)
		}
		if c.docArgs != "" && strings.Join(got.docArgs(), " ") != c.docArgs {
			t.Errorf("%q: doc args %q", c.args, got.docArgs())
		}
	}
	got := parseCargoTest(split("+stable -q"), split("-p a"))
	if !got.Quiet || strings.Join(got.buildArgs(), " ") != "+stable -q test --no-run --message-format=json-render-diagnostics -p a" {
		t.Fatalf("%+v %q", got, got.buildArgs())
	}
	if got := parseCargoTest(split("--color always"), nil); got.Color != "always" {
		t.Fatal(got.Color)
	}
	if got := parseCargoTest(nil, split("--manifest-path x/Cargo.toml --color=never")); got.Manifest != "x/Cargo.toml" || got.Color != "never" {
		t.Fatalf("%+v", got)
	}
}

func TestParseNextest(t *testing.T) {
	cases := []struct {
		args, archive, run string
		ok, unsplit        bool
	}{
		{args: "run", ok: true, archive: "nextest archive --archive-file F", run: "nextest run --archive-file F"},
		{args: "r -p app-store foo", ok: true, archive: "nextest archive --archive-file F -p app-store", run: "nextest r --archive-file F foo"},
		{args: "run --workspace --all-features --cargo-profile ci -E test(x) --no-fail-fast -j 4 --retries=2 -P ci --release -- --exact a",
			ok: true, archive: "nextest archive --archive-file F --workspace --all-features --cargo-profile ci -P ci --release",
			run: "nextest run --archive-file F -E test(x) --no-fail-fast -j 4 --retries=2 -P ci -- --exact a"},
		{args: "--color always run -Fx --features=y --target-dir t --manifest-path m/Cargo.toml --partition count:1/2 --run-ignored all", ok: true,
			archive: "nextest --color always archive --archive-file F -Fx --features=y --target-dir t --manifest-path m/Cargo.toml",
			run:     "nextest --color always run --archive-file F --partition count:1/2 --run-ignored all"},
		{args: "run --no-capture --profile=ci --config-file c.toml", ok: true,
			archive: "nextest archive --archive-file F --profile=ci --config-file c.toml",
			run:     "nextest run --archive-file F --no-capture --profile=ci --config-file c.toml"},
		{args: "list", ok: false},
		{args: "archive --archive-file x", ok: false},
		{args: "run --no-run", ok: true, unsplit: true},
		{args: "run --archive-file x.tar.zst", ok: true, unsplit: true},
		{args: "run --workspace-remap .", ok: true, unsplit: true},
	}
	for _, c := range cases {
		n, ok := parseNextest(nil, split(c.args))
		if ok != c.ok {
			t.Errorf("%q: ok=%v", c.args, ok)
			continue
		}
		if !ok {
			continue
		}
		if (n.Unsplit != "") != c.unsplit {
			t.Errorf("%q: unsplit=%q", c.args, n.Unsplit)
			continue
		}
		if c.unsplit {
			continue
		}
		if a := strings.Join(n.archiveArgs("F"), " "); a != c.archive {
			t.Errorf("%q: archive\n got %q\nwant %q", c.args, a, c.archive)
		}
		if r := strings.Join(n.runArgs("F"), " "); r != c.run {
			t.Errorf("%q: run\n got %q\nwant %q", c.args, r, c.run)
		}
	}
}

func TestPlanSplit(t *testing.T) {
	cargo := config.Tool{}
	if p := planSplit(cargo, "cargo", split("test -p a")); p == nil || p.kind != splitCargoTest {
		t.Fatalf("cargo test: %+v", p)
	}
	if p := planSplit(cargo, "cargo", split("nextest run -p a foo")); p == nil || p.kind != splitNextest {
		t.Fatalf("nextest: %+v", p)
	}
	for _, args := range []string{"build", "test --no-run", "nextest list", "nextest run --no-run", "check --tests"} {
		if p := planSplit(cargo, "cargo", split(args)); p != nil {
			t.Errorf("%q split", args)
		}
	}
	if p := planSplit(cargo, "npm", split("test")); p != nil {
		t.Error("npm test split")
	}
	only := []string{"nextest"}
	if p := planSplit(config.Tool{Split: &only}, "cargo", split("test")); p != nil {
		t.Error("cargo test split although only nextest is enabled")
	}
	none := []string{}
	if p := planSplit(config.Tool{Split: &none}, "cargo", split("nextest run")); p != nil {
		t.Error("split = [] still splits")
	}
	t.Setenv("CLOUDSOURCED_SPLIT", "0")
	if p := planSplit(cargo, "cargo", split("test")); p != nil {
		t.Error("CLOUDSOURCED_SPLIT=0 still splits")
	}
}

func TestLineFilter(t *testing.T) {
	var b strings.Builder
	f := newLineFilter(&b, "Archiving ", "Archived ")
	f.Write([]byte("    Finished `test` profile\n   Archiv"))
	f.Write([]byte("ing 5 binaries\n\x1b[1m\x1b[92m    Archived\x1b[0m 8 files\n\r   Building [==> ] 1/2\rdone"))
	f.Flush()
	if got := b.String(); got != "    Finished `test` profile\n\r   Building [==> ] 1/2\rdone" {
		t.Fatalf("%q", got)
	}
}

func TestInsideBuildPaths(t *testing.T) {
	for _, c := range []struct {
		abs string
		rel string
		ok  bool
	}{
		{"/r/target/debug/deps/a-1", "target/debug/deps/a-1", true},
		{"/r/app/target/debug/x", "app/target/debug/x", true},
		{"/r/src/x", "", false},
		{"/elsewhere/target/x", "", false},
	} {
		rel, ok := insideBuildPaths("/r", []string{"target", "app/target/"}, c.abs)
		if rel != c.rel || ok != c.ok {
			t.Errorf("%s: %q %v", c.abs, rel, ok)
		}
	}
	if escapeGlob("target/a[1]*?{x}") != `target/a\[1\]\*\?\{x\}` {
		t.Fatal(escapeGlob("target/a[1]*?{x}"))
	}
}

func TestPackageFromID(t *testing.T) {
	if p := packageFromID("path+file:///w/a#0.1.0", "/w/a/Cargo.toml"); p.Name != "a" || p.Version != "0.1.0" {
		t.Fatalf("%+v", p)
	}
	if p := packageFromID("path+file:///w/crates/x#app-store@1.2.3-rc.1", ""); p.Name != "app-store" || p.Version != "1.2.3-rc.1" {
		t.Fatalf("%+v", p)
	}
}

func TestParseTestBuildDoctests(t *testing.T) {
	msg := func(pkg, kind string, doctest, test bool, exe string) string {
		e := "null"
		if exe != "" {
			e = `"` + exe + `"`
		}
		return `{"reason":"compiler-artifact","package_id":"path+file:///w/` + pkg + `#0.1.0","manifest_path":"/w/` + pkg + `/Cargo.toml",` +
			`"target":{"kind":["` + kind + `"],"name":"` + pkg + `","src_path":"/w/` + pkg + `/src/x.rs","doctest":` + strconv.FormatBool(doctest) + `},` +
			`"profile":{"test":` + strconv.FormatBool(test) + `},"executable":` + e + `}`
	}
	// A bin-only package with a path dependency that has a library.
	in := strings.Join([]string{
		msg("macros", "proc-macro", true, false, ""),
		msg("hello", "bin", false, true, "/w/target/debug/deps/hello-1"),
		msg("hello", "bin", false, false, "/w/target/debug/hello"),
		"plain output",
	}, "\n")
	b, rest := parseTestBuild(strings.NewReader(in), nil)
	if b.doctests || len(b.units) != 1 || len(b.exes) != 2 || string(rest) != "plain output\n" {
		t.Fatalf("%+v %q", b, rest)
	}
	if b.bins["/w/hello/Cargo.toml"]["hello"] != "/w/target/debug/hello" {
		t.Fatalf("bins %+v", b.bins)
	}
	in += "\n" + msg("macros", "proc-macro", true, true, "/w/target/debug/deps/macros-1")
	if b, _ := parseTestBuild(strings.NewReader(in), nil); !b.doctests || b.units[0].pkg.Name != "hello" || b.units[1].kind != "lib" {
		t.Fatalf("%+v", b)
	}
}
