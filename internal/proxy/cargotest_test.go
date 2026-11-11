package proxy

import (
	"bytes"
	"github.com/traumventures/cloudsourced/internal/platform"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// copyTree copies the fixture workspace into dir.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var (
	reTimes   = regexp.MustCompile(`(finished in|in) [0-9.]+m?s`)
	reThreads = regexp.MustCompile(`thread '([^']*)' \(\d+\)`)
)

func normalize(s string) string {
	s = reTimes.ReplaceAllString(s, "$1 T")
	s = reThreads.ReplaceAllString(s, "thread '$1'")
	var keep []string
	for _, l := range strings.SplitAfter(s, "\n") {
		if strings.Contains(l, "Finished `test` profile") || strings.Contains(l, "Compiling ") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "")
}

// TestCargoTestRunnerMatchesCargo builds a small workspace with the local
// cargo (standing in for the remote build phase) and checks that running
// the binaries like cargo test produces the same stdout, stderr and exit
// code as `cargo test` itself.
func TestCargoTestRunnerMatchesCargo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a cargo workspace")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed")
	}
	ws := t.TempDir()
	copyTree(t, "testdata/splitws", ws)
	t.Setenv("CARGO_TARGET_DIR", filepath.Join(ws, "target"))
	t.Setenv("CARGO_TERM_COLOR", "never")
	t.Setenv("RUST_BACKTRACE", "0")
	t.Setenv("RUST_TEST_THREADS", "1")
	cases := []struct {
		name, dir, args string
		fail            bool
	}{
		{name: "workspace", dir: ".", args: "--lib --bins --tests -- --nocapture"},
		{name: "filter", dir: ".", args: "--tests lib_env -- --nocapture"},
		{name: "package dir", dir: "a", args: "--tests"},
		{name: "quiet", dir: ".", args: "-q --tests"},
		{name: "failure stops", dir: ".", args: "--tests", fail: true},
		{name: "failure no-fail-fast", dir: ".", args: "--tests --no-fail-fast", fail: true},
		{name: "failure in package dir", dir: "a", args: "--lib", fail: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := filepath.Join(ws, c.dir)
			if c.fail {
				t.Setenv("SPLIT_FAIL", "1")
			}
			args := strings.Fields(c.args)
			var wantOut, wantErr bytes.Buffer
			cmd := exec.Command("cargo", append([]string{"test"}, args...)...)
			cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, &wantOut, &wantErr
			wantCode := 0
			if err := cmd.Run(); err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				wantCode = ee.ExitCode()
			}

			pt := parseCargoTest(nil, args)
			if pt.Unsplit != "" || pt.DocTests {
				t.Fatalf("unexpected plan %+v", pt)
			}
			build := exec.Command("cargo", pt.buildArgs()...)
			build.Dir = cwd
			js, err := build.Output()
			if err != nil {
				t.Fatal(err)
			}
			b, rest := parseTestBuild(bytes.NewReader(js), cargoMetadata(cwd, pt))
			if len(rest) > 0 {
				t.Fatalf("non-JSON output %q", rest)
			}
			if why := platform.CheckRun(b.exes); why != "" {
				t.Fatalf("locally built binaries rejected: %s", why)
			}
			var gotOut, gotErr bytes.Buffer
			r := &testRunner{t: pt, cwd: cwd, color: false, current: currentManifest(cwd, ""), out: &gotErr, stdout: &gotOut}
			failed, _ := r.run(b)
			gotCode := 0
			if len(failed) > 0 {
				if pt.NoFail {
					r.summary(failed)
				}
				gotCode = 101
			}
			if gotCode != wantCode {
				t.Errorf("exit code %d, cargo %d", gotCode, wantCode)
			}
			if g, w := normalize(gotOut.String()), normalize(wantOut.String()); g != w {
				t.Errorf("stdout differs\n--- got\n%s\n--- cargo\n%s", g, w)
			}
			if g, w := normalize(gotErr.String()), normalize(wantErr.String()); g != w {
				t.Errorf("stderr differs\n--- got\n%s\n--- cargo\n%s", g, w)
			}
		})
	}
}
