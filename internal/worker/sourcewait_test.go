package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A source fault waits for the client to reconnect (a root attached
// again) instead of failing at once, and fails after the bounded wait.
func TestWaitSourceWakesAndTimesOut(t *testing.T) {
	s := &Worker{cfg: Config{SourceWait: 300 * time.Millisecond}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sourceWake: make(chan struct{})}
	var start time.Time
	wake := s.sourceWake
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.mu.Lock()
		close(s.sourceWake)
		s.sourceWake = make(chan struct{})
		s.mu.Unlock()
	}()
	cause := errors.New("timeout: no recent network activity")
	if err := s.waitSource(context.Background(), "/r", wake, &start, cause, nil); err != nil {
		t.Fatalf("not woken by the reconnect: %v", err)
	}
	// The wait is measured from the first failure, across retries.
	t0 := time.Now()
	err := s.waitSource(context.Background(), "/r", s.sourceWake, &start, cause, nil)
	if err == nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(t0); d > 400*time.Millisecond {
		t.Fatalf("waited %s", d)
	}
	if err := s.waitSource(context.Background(), "/r", s.sourceWake, &start, cause, nil); err == nil {
		t.Fatal("expired wait succeeded")
	}
}
