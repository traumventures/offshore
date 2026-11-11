package worker

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
)

// TestStatusSessionLabels: the labels of a session's Exec request are
// reported (cleaned) in the machine status.
func TestStatusSessionLabels(t *testing.T) {
	sl, err := newSlots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Worker{cfg: Config{MachineID: "m1"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: map[string]*session{}, slots: sl}
	s.sessions["s1"] = &session{id: "s1", started: time.Now(), done: make(chan struct{}),
		req: proto.Exec{Lease: "l1", Argv: []string{"cargo", "build"}, Labels: map[string]string{"agent": "codex", "worktree": "/w/a", "Bad": "x"}}}
	st := s.statusOnly()
	if len(st.Sessions) != 1 || st.Sessions[0].Labels["agent"] != "codex" || st.Sessions[0].Labels["worktree"] != "/w/a" || st.Sessions[0].Labels["Bad"] != "" {
		t.Fatalf("sessions %+v", st.Sessions)
	}
}
