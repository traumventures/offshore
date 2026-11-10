package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// TestForward checks a forwarded connection end to end over QUIC: echo
// through the session, refusal for unknown sessions and teardown when the
// session's process exits.
func TestForward(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	port := echo.Addr().(*net.TCPAddr).Port

	id, err := transport.NewIdentity("cs-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{Token: "tok", Identity: *id}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: map[string]*session{}}
	se := &session{id: "s1", done: make(chan struct{})}
	s.sessions[se.id] = se
	ln, err := transport.Listen("127.0.0.1:0", &s.cfg.Identity)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			go s.serveConn(ctx, c)
		}
	}()
	conn, err := transport.Dial(ctx, ln.Addr().String(), id.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")

	open := func(session string, port int) (*proto.Conn, io.ReadWriteCloser, error) {
		st, pc, err := transport.OpenStream(ctx, conn, "tok", proto.KindForward)
		if err != nil {
			return nil, nil, err
		}
		if err := pc.Send(proto.TForward, proto.Forward{Session: session, Port: port}); err != nil {
			return nil, nil, err
		}
		return pc, st, pc.Expect(proto.TAck, nil)
	}
	if _, _, err := open("nope", port); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("unknown session: %v", err)
	}
	_, st, err := open("s1", port)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(st, "ping\n")
	line, err := bufio.NewReader(st).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo: %q %v", line, err)
	}
	se.end()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, st); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward not torn down when the session ended")
	}
}
