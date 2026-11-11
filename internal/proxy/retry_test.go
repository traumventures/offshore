package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
)

func TestRetryable(t *testing.T) {
	gone := func() bool { return false }
	alive := func() bool { return true }
	state := func(f func(*attemptState)) *attemptState {
		st := &attemptState{}
		st.started.Store(true)
		if f != nil {
			f(st)
		}
		return st
	}
	cases := []struct {
		name     string
		st       *attemptState
		attempt  int
		disabled bool
		probe    func() bool
		want     bool
	}{
		{"lost while running", state(nil), 1, false, gone, true},
		{"second loss", state(nil), 2, false, gone, false},
		{"disabled", state(nil), 1, true, gone, false},
		{"machine still answers", state(nil), 1, false, alive, false},
		{"not started", &attemptState{}, 1, false, gone, false},
		{"write-back begun", state(func(s *attemptState) { s.writeback.Store(true) }), 1, false, gone, false},
		{"stdin consumed", state(func(s *attemptState) { s.stdinUsed.Store(true) }), 1, false, gone, false},
		{"interrupted", state(func(s *attemptState) { s.interrupted.Store(true) }), 1, false, gone, false},
	}
	for _, c := range cases {
		probed := false
		probe := func() bool { probed = true; return c.probe() }
		got, why := retryable(c.st, c.attempt, c.disabled, probe)
		if got != c.want {
			t.Errorf("%s: %v (%s), want %v", c.name, got, why, c.want)
		}
		if !got && why == "" {
			t.Errorf("%s: no reason given", c.name)
		}
		// The probe (a network dial) only runs when nothing else rules a
		// retry out.
		if probed && c.name != "lost while running" && c.name != "machine still answers" {
			t.Errorf("%s: probed needlessly", c.name)
		}
	}
}

// fakeSession runs a worker side on a pipe: it reads the proxy's frames in
// the background and runs script.
func fakeSession(t *testing.T, script func(*proto.Conn, net.Conn)) *proto.Conn {
	t.Helper()
	a, b := net.Pipe()
	srv := proto.NewConn(b)
	go func() {
		go func() { // drain stdin/signal frames
			for {
				if _, _, err := srv.Read(); err != nil {
					return
				}
			}
		}()
		script(srv, b)
	}()
	t.Cleanup(func() { a.Close(); b.Close() })
	return proto.NewConn(a)
}

func TestStreamClassifiesLostConnection(t *testing.T) {
	t.Setenv("CLOUDSOURCED_STDIN", "never")
	pc := fakeSession(t, func(srv *proto.Conn, raw net.Conn) {
		srv.Write(proto.TStdout, []byte("partial output\n"))
		raw.Close() // the machine disappears mid-run
	})
	st := &attemptState{}
	_, err := stream(pc, false, false, io.Discard, io.Discard, st)
	var perr *proto.Error
	if err == nil || errors.As(err, &perr) {
		t.Fatalf("want a transport error, got %v", err)
	}
	if st.stdinUsed.Load() || st.interrupted.Load() {
		t.Fatal("state polluted")
	}

	// An error reported by the worker is not a lost machine.
	pc = fakeSession(t, func(srv *proto.Conn, _ net.Conn) {
		srv.SendError("exec", errors.New("out of memory"))
	})
	_, err = stream(pc, false, false, io.Discard, io.Discard, &attemptState{})
	if !errors.As(err, &perr) {
		t.Fatalf("want *proto.Error, got %v", err)
	}

	// A normal exit.
	pc = fakeSession(t, func(srv *proto.Conn, _ net.Conn) {
		srv.Send(proto.TExit, proto.Exit{Code: 3})
	})
	if ex, err := stream(pc, false, false, io.Discard, io.Discard, &attemptState{}); err != nil || ex.Code != 3 {
		t.Fatalf("%+v %v", ex, err)
	}
}

// Forwarded standard input cannot be replayed on another machine.
func TestStreamRecordsStdinUse(t *testing.T) {
	t.Setenv("CLOUDSOURCED_STDIN", "always")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("input"))
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	pc := fakeSession(t, func(srv *proto.Conn, raw net.Conn) {
		time.Sleep(100 * time.Millisecond) // let the proxy forward stdin
		raw.Close()
	})
	st := &attemptState{}
	stream(pc, false, false, io.Discard, io.Discard, st)
	if !st.stdinUsed.Load() {
		t.Fatal("stdin use not recorded")
	}
	if ok, _ := retryable(st, 1, false, func() bool { return false }); ok {
		t.Fatal("retry after consumed stdin")
	}
}

func TestMachineLostError(t *testing.T) {
	base := errors.New("timeout: no recent network activity")
	err := error(&machineLost{machine: "m1", err: base})
	if !errors.Is(err, base) || err.Error() != "remote session: connection to machine m1 lost: timeout: no recent network activity" {
		t.Fatalf("%v", err)
	}
}
