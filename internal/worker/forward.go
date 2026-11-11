package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/transport"
)

// forwardDialWindow is how long a forwarded connection retries while the
// session's server is not listening yet (dev servers start after the
// session).
const forwardDialWindow = 5 * time.Second

// serveForward connects one forwarded client TCP connection to
// 127.0.0.1:port. Sessions share the machine's network namespace (and so
// do service containers), so the server dials from its own namespace. The
// connection is torn down when the session's process exits.
func (s *Worker) serveForward(ctx context.Context, pc *proto.Conn, st *quic.Stream, claims auth.Claims) error {
	var req proto.Forward
	if err := pc.Expect(proto.TForward, &req); err != nil {
		return err
	}
	s.mu.Lock()
	se := s.sessions[req.Session]
	s.mu.Unlock()
	if se == nil {
		return pc.SendError("forward", fmt.Errorf("session %s is not running", req.Session))
	}
	// Only the lease that started a session may reach into it.
	if !claims.AllowsLease(se.req.Lease) {
		return pc.SendError("auth", fmt.Errorf("%w: token is not valid for session %s", auth.ErrInvalidToken, req.Session))
	}
	if req.Port < 1 || req.Port > 65535 {
		return pc.SendError("forward", fmt.Errorf("invalid port %d", req.Port))
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port))
	var c net.Conn
	var err error
	deadline := time.Now().Add(forwardDialWindow)
	for {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		c, err = d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		select {
		case <-se.done:
			return pc.SendError("forward", errors.New("session ended"))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil {
		return pc.SendError("forward", err)
	}
	defer c.Close()
	if err := pc.Write(proto.TAck, nil); err != nil {
		return err
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-se.done:
		case <-ctx.Done():
		case <-stop:
			return
		}
		c.Close()
		st.CancelRead(0)
		st.CancelWrite(0)
	}()
	defer close(stop)
	transport.Splice(c, st)
	return nil
}
