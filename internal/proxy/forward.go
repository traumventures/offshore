package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// ports returns the ports to forward for a tool: CLOUDSOURCED_PORTS
// replaces the repository's [tool.X] ports when set.
func ports(profile config.Tool) ([]config.Port, error) {
	if env := os.Getenv("CLOUDSOURCED_PORTS"); env != "" {
		ps, err := config.ParsePortsEnv(env)
		if err != nil {
			return nil, fmt.Errorf("CLOUDSOURCED_PORTS: %w", err)
		}
		return ps, nil
	}
	return config.ParsePorts(profile.Ports)
}

// forwarder listens on local ports and forwards each accepted connection
// over its own QUIC stream into the running session.
type forwarder struct {
	ports []config.Port
	lns   []net.Listener

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	active map[net.Conn]*quic.Stream
	warned map[int]bool
}

// listenPorts binds 127.0.0.1:<local> for every port. Ports that cannot be
// bound are reported and skipped; the run continues without them.
func listenPorts(ps []config.Port) *forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	f := &forwarder{ctx: ctx, cancel: cancel, active: map[net.Conn]*quic.Stream{}, warned: map[int]bool{}}
	for _, p := range ps {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Local)))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				warn("cannot forward port %s: local port %d is already in use (continuing without it)", p, p.Local)
			} else {
				warn("cannot forward port %s: %v (continuing without it)", p, err)
			}
			continue
		}
		f.ports = append(f.ports, p)
		f.lns = append(f.lns, ln)
	}
	return f
}

// forwarded lists the ports that are actually forwarded.
func (f *forwarder) forwarded() []string {
	var out []string
	for _, p := range f.ports {
		out = append(out, p.String())
	}
	return out
}

// serve starts accepting connections for session on conn.
func (f *forwarder) serve(conn *quic.Conn, token, session string) {
	for i, ln := range f.lns {
		p := f.ports[i]
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				f.wg.Add(1)
				go func() {
					defer f.wg.Done()
					f.forward(c, conn, token, session, p)
				}()
			}
		}()
	}
}

func (f *forwarder) forward(c net.Conn, conn *quic.Conn, token, session string, p config.Port) {
	defer c.Close()
	st, pc, err := transport.OpenStream(f.ctx, conn, token, proto.KindForward)
	if err != nil {
		return
	}
	defer st.Close()
	if !f.track(c, st) {
		return
	}
	defer f.untrack(c)
	if err := pc.Send(proto.TForward, proto.Forward{Session: session, Port: p.Remote}); err != nil {
		return
	}
	if err := pc.Expect(proto.TAck, nil); err != nil {
		f.mu.Lock()
		first := !f.warned[p.Local]
		f.warned[p.Local] = true
		f.mu.Unlock()
		if first && f.ctx.Err() == nil {
			warn("port %s: remote 127.0.0.1:%d: %v", p, p.Remote, err)
		}
		return
	}
	transport.Splice(c, st)
}

func (f *forwarder) track(c net.Conn, st *quic.Stream) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		return false
	}
	f.active[c] = st
	return true
}

func (f *forwarder) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.active, c)
	f.mu.Unlock()
}

// close stops listening and tears down active connections.
func (f *forwarder) close() {
	f.mu.Lock()
	f.cancel()
	for c, st := range f.active {
		c.Close()
		st.CancelRead(0)
		st.CancelWrite(0)
	}
	f.mu.Unlock()
	for _, ln := range f.lns {
		ln.Close()
	}
	f.wg.Wait()
}
