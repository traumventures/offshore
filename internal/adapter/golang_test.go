package adapter

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseGoTest(t *testing.T) {
	cases := []struct {
		args             string
		build, pkgs, bin string
		verbose          bool
		unsplit          bool
	}{
		{args: "", pkgs: "."},
		{args: "./...", pkgs: "./..."},
		{args: "-race -tags=integration -run TestX -v ./a ./b", build: "-race -tags=integration", pkgs: "./a ./b", bin: "-test.run=TestX -test.v", verbose: true},
		{args: "-count=1 ./a -args -flag x", pkgs: "./a", bin: "-test.count=1 -flag x"},
		{args: "./a -timeout 30s -short", pkgs: "./a", bin: "-test.timeout=30s -test.short"},
		{args: "-v=false ./a", pkgs: "./a", bin: "-test.v=false"},
		{args: "-json ./...", unsplit: true},
		{args: "-cover ./...", unsplit: true},
		{args: "-o x.test ./a", unsplit: true},
		{args: "-bogus ./a", unsplit: true},
	}
	for _, c := range cases {
		g := parseGoTest(strings.Fields(c.args))
		if (g.Unsplit != "") != c.unsplit {
			t.Errorf("%q: unsplit %q", c.args, g.Unsplit)
			continue
		}
		if c.unsplit {
			continue
		}
		if strings.Join(g.Build, " ") != c.build || strings.Join(g.Packages, " ") != c.pkgs || strings.Join(g.Binary, " ") != c.bin || g.Verbose != c.verbose {
			t.Errorf("%q: %+v", c.args, g)
		}
	}
}

func TestParseGoList(t *testing.T) {
	got := parseGoList(strings.NewReader("ex/a\t/w/a\t/o/1.test\nex/c\t/w/c\t\nbad line\n"))
	want := []goTestPkg{{"ex/a", "/w/a", "/o/1.test"}, {"ex/c", "/w/c", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestHoldLast(t *testing.T) {
	var b bytes.Buffer
	h := &holdLast{w: &b}
	h.Write([]byte("=== RUN x\n--- PA"))
	h.Write([]byte("SS: x\nPASS\n"))
	if b.String() != "=== RUN x\n--- PASS: x\n" || h.last != "PASS\n" {
		t.Fatalf("%q %q", b.String(), h.last)
	}
	h.flush()
	if !strings.HasSuffix(b.String(), "PASS\n") {
		t.Fatal(b.String())
	}
}

var reGoTimes = regexp.MustCompile(`\d+\.\d+s`)

// TestGoRunnerMatchesGoTest builds a module with the remote phase's
// script locally and checks that the local run prints what go test prints.
func TestGoRunnerMatchesGoTest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds go test binaries")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	mod := t.TempDir()
	files := map[string]string{
		"go.mod":      "module ex\n\ngo 1.21\n",
		"a/a_test.go": "package a\nimport \"testing\"\nfunc TestA(t *testing.T){t.Log(\"hi\")}\n",
		"b/b_test.go": "package b\nimport (\"os\";\"testing\")\nfunc TestB(t *testing.T){if os.Getenv(\"GO_SPLIT_FAIL\")!=\"\"{t.Fatal(\"bad\")}}\n",
		"c/c.go":      "package c\nfunc C(){}\n",
	}
	for p, s := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(mod, p)), 0o755)
		os.WriteFile(filepath.Join(mod, p), []byte(s), 0o644)
	}
	for _, c := range []struct {
		name, args string
		fail       bool
	}{
		{"all", "./...", false},
		{"verbose", "-v ./a", false},
		{"failure", "./...", true},
		{"failure verbose", "-v -run TestB ./b", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.fail {
				t.Setenv("GO_SPLIT_FAIL", "1")
			}
			args := strings.Fields(c.args)
			want := exec.Command("go", append([]string{"test", "-count=1"}, args...)...)
			want.Dir = mod
			wantOut, err := want.CombinedOutput()
			wantCode := 0
			if ee, ok := err.(*exec.ExitError); ok {
				wantCode = ee.ExitCode()
			}
			g := parseGoTest(args)
			out := filepath.Join(t.TempDir(), "o")
			build := exec.Command("sh", "-c", g.buildScript(out))
			build.Dir = mod
			if b, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, b)
			}
			list, _ := os.ReadFile(filepath.Join(out, "list"))
			var got bytes.Buffer
			failed, _ := (&goRunner{verbose: g.Verbose, args: g.Binary, out: &got}).run(parseGoList(bytes.NewReader(list)))
			gotCode := 0
			if failed {
				gotCode = 1
			}
			if gotCode != wantCode {
				t.Errorf("exit %d, go test %d", gotCode, wantCode)
			}
			if g, w := reGoTimes.ReplaceAllString(got.String(), "T"), reGoTimes.ReplaceAllString(string(wantOut), "T"); g != w {
				t.Errorf("output differs\n--- got\n%s\n--- go test\n%s", g, w)
			}
		})
	}
}
