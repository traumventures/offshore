package runlog

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRingKeepsLastRuns(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3*Keep+7; i++ {
		if err := Append(dir, Run{Time: time.Unix(int64(i), 0), Tool: fmt.Sprint(i), Mode: Local}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := Recent(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != Keep {
		t.Fatalf("got %d runs, want %d", len(runs), Keep)
	}
	if runs[0].Tool != fmt.Sprint(3*Keep+6) || runs[Keep-1].Tool != fmt.Sprint(2*Keep+7) {
		t.Fatalf("wrong order or window: first %s last %s", runs[0].Tool, runs[Keep-1].Tool)
	}
	data, _ := os.ReadFile(Path(dir))
	if n := strings.Count(string(data), "\n"); n > 2*Keep {
		t.Fatalf("file not bounded: %d lines", n)
	}
	last, _ := Recent(dir, 2)
	if len(last) != 2 || last[0].Tool != runs[0].Tool {
		t.Fatalf("Recent(2) = %+v", last)
	}
}

func TestConcurrentAppendAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	if runs, err := Recent(dir, 1); err != nil || len(runs) != 0 {
		t.Fatalf("empty log: %v %v", runs, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := i
			Append(dir, Run{Tool: "t", Mode: Remote, Exit: &code, Cache: map[string]int64{"hits": int64(i)}})
		}(i)
	}
	wg.Wait()
	runs, _ := Recent(dir, 0)
	if len(runs) != 40 {
		t.Fatalf("got %d runs", len(runs))
	}
	for _, r := range runs {
		if r.Exit == nil || r.Cache["hits"] != int64(*r.Exit) {
			t.Fatalf("corrupt run %+v", r)
		}
	}
}
