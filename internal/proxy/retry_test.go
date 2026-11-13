package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/scheduler"
)

func TestRetryable(t *testing.T) {
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
		want     bool
	}{
		{"lost while running", state(nil), 1, false, true},
		{"second loss", state(nil), 2, false, false},
		{"disabled", state(nil), 1, true, false},
		{"not started", &attemptState{}, 1, false, false},
		{"write-back begun", state(func(s *attemptState) { s.writeback.Store(true) }), 1, false, false},
		{"stdin consumed", state(func(s *attemptState) { s.stdinUsed.Store(true) }), 1, false, false},
		{"interrupted", state(func(s *attemptState) { s.interrupted.Store(true) }), 1, false, false},
	}
	for _, c := range cases {
		got, why := retryable(c.st, c.attempt, c.disabled)
		if got != c.want {
			t.Errorf("%s: %v (%s), want %v", c.name, got, why, c.want)
		}
		if !got && why == "" {
			t.Errorf("%s: no reason given", c.name)
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
	t.Setenv("OFFSHORE_STDIN", "never")
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
	t.Setenv("OFFSHORE_STDIN", "always")
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
	if ok, _ := retryable(st, 1, false); ok {
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

// OOM kills and worker shutdowns under a running command are retried;
// other failures and normal exits are not.
func TestKilledByMachine(t *testing.T) {
	cases := []struct {
		code int
		u    scheduler.Usage
		want bool
	}{
		{137, scheduler.Usage{OOM: true, Killer: "cgroup_oom"}, true},
		{101, scheduler.Usage{OOM: true, Killer: "cgroup_oom"}, true}, // rustc killed, cargo reports 101
		{137, scheduler.Usage{Signal: 9, Killer: "worker_shutdown"}, true},
		{130, scheduler.Usage{Signal: 2, Killer: "client_signal"}, false},
		{101, scheduler.Usage{}, false},
		{0, scheduler.Usage{OOM: true}, false},
	}
	for _, c := range cases {
		if got := killedByMachine(c.code, c.u); got != c.want {
			t.Errorf("%d %+v: %v, want %v", c.code, c.u, got, c.want)
		}
	}
	if s := (&sessionKilled{machine: "m1", oom: true, peak: 3 << 30, what: killedProcess("cargo")}).Error(); s != "rustc killed by OOM on a shared machine m1 (peak 3.0 GiB)" {
		t.Errorf("%q", s)
	}
}
