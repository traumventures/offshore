package server

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/traumventures/cloudsourced/internal/machine"
)

func TestHostDrainNotice(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{MachineID: "m1", CacheDir: t.TempDir(), HostStatus: filepath.Join(dir, "status.json")},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.checkDrainNotice()
	if st := s.statusOnly(); st.Draining || st.InterruptAt != 0 {
		t.Fatalf("draining without a notice: %+v", st)
	}
	if err := machine.WriteDrainNotice(dir, machine.DrainNotice{Reason: machine.DrainSpotRebalance, Noticed: 1000}); err != nil {
		t.Fatal(err)
	}
	s.checkDrainNotice()
	if st := s.statusOnly(); !st.Draining || st.DrainReason != machine.DrainSpotRebalance || st.InterruptAt != 1000 {
		t.Fatalf("rebalance: %+v", st)
	}
	if err := machine.WriteDrainNotice(dir, machine.DrainNotice{Reason: machine.DrainSpotInterruption, Deadline: 1120, Noticed: 1001}); err != nil {
		t.Fatal(err)
	}
	s.checkDrainNotice()
	if st := s.statusOnly(); !st.Draining || st.DrainReason != machine.DrainSpotInterruption || st.InterruptAt != 1120 {
		t.Fatalf("interruption: %+v", st)
	}
}
