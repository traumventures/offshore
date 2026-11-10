package awsec2

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The reaper Lambda is inline in stack.yaml (ZipFile); its source with
// tests lives in reaper/. Both must be identical.
func TestReaperInlineMatchesSource(t *testing.T) {
	src, err := os.ReadFile("reaper/reaper.py")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "        ZipFile: |\n"
	tpl := string(stackTemplate)
	i := strings.Index(tpl, marker)
	if i < 0 {
		t.Fatal("ZipFile block not found")
	}
	var got []string
	for _, line := range strings.Split(tpl[i+len(marker):], "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		got = append(got, strings.TrimPrefix(line, "          "))
	}
	inline := strings.TrimRight(strings.Join(got, "\n"), "\n") + "\n"
	if inline != string(src) {
		t.Fatalf("stack.yaml ZipFile differs from reaper/reaper.py; copy the file into the block indented by 10 spaces\n--- inline\n%s", inline)
	}
	if len(src) > 4096 {
		t.Fatalf("inline Lambda code is %d bytes, CloudFormation allows 4096", len(src))
	}
}

func TestReaperPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(py, "reaper/reaper_test.py").CombinedOutput()
	if err != nil {
		t.Fatalf("reaper tests failed: %v\n%s", err, out)
	}
}
