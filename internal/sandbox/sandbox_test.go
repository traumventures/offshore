package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetOOMScoreAdj(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oom_score_adj")
	old := oomScoreAdjPath
	oomScoreAdjPath = p
	defer func() { oomScoreAdjPath = old }()
	if err := setOOMScoreAdj(SessionOOMScoreAdj); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "500" {
		t.Fatalf("got %q", b)
	}
}
