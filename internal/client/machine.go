package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// machineConn is the daemon's long-lived connection to one machine. It
// carries one sync stream per attached root and serves blob requests.
type machineConn struct {
	id, addr, fingerprint, token string
	d                            *Daemon
	log                          *slog.Logger

	mu    sync.Mutex
	conn  *quic.Conn
	roots map[string]*rootSync
}

// rootSync streams one root's manifest to one machine.
type rootSync struct {
	mc   *machineConn
	root *root

	mu      sync.Mutex
	queue   []manifest.Delta
	wake    chan struct{}
	acked   uint64
	ackCh   chan struct{}
	failed  error
	stopped bool
}

func (s *rootSync) push(d manifest.Delta) {
	s.mu.Lock()
	s.queue = append(s.queue, d)
	s.mu.Unlock()
	s.kick()
}

func (s *rootSync) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// waitAck blocks until the machine applied version v.
func (s *rootSync) waitAck(ctx context.Context, v uint64) error {
	for {
		s.mu.Lock()
		acked, ch, failed := s.acked, s.ackCh, s.failed
		s.mu.Unlock()
		if failed != nil {
			return failed
		}
		if acked >= v {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *Daemon) machineConn(ctx context.Context, id, addr, fingerprint, token string) (*machineConn, error) {
	d.mu.Lock()
	mc, ok := d.conns[id]
	if ok && mc.addr != addr {
		// Machine restarted with a new address.
		mc.close()
		delete(d.conns, id)
		ok = false
	}
	if !ok {
		mc = &machineConn{id: id, addr: addr, fingerprint: fingerprint, token: token, d: d, log: d.log, roots: map[string]*rootSync{}}
		d.conns[id] = mc
	}
	d.mu.Unlock()
	return mc, mc.ensureConn(ctx)
}

func (mc *machineConn) ensureConn(ctx context.Context) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.conn != nil && mc.conn.Context().Err() == nil {
		return nil
	}
	for _, rs := range mc.roots {
		rs.stop(errors.New("connection lost"))
	}
	mc.roots = map[string]*rootSync{}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(dctx, mc.addr, mc.fingerprint)
	if err != nil {
		return fmt.Errorf("connect to machine %s: %w", mc.id, err)
	}
	mc.conn = conn
	go mc.serveBlobs(conn)
	return nil
}

func (mc *machineConn) close() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	for _, rs := range mc.roots {
		rs.stop(errors.New("closed"))
	}
	if mc.conn != nil {
		mc.conn.CloseWithError(0, "")
	}
}

func (rs *rootSync) stop(err error) {
	rs.mu.Lock()
	if rs.stopped {
		rs.mu.Unlock()
		return
	}
	rs.stopped = true
	if rs.failed == nil {
		rs.failed = err
	}
	close(rs.ackCh)
	rs.ackCh = make(chan struct{})
	rs.mu.Unlock()
	rs.root.unsubscribe(rs)
	rs.kick()
}

// attach makes sure the root is projected on the machine and returns its
// sync stream.
func (mc *machineConn) attach(ctx context.Context, r *root, workerLocal []string) (*rootSync, error) {
	if err := mc.ensureConn(ctx); err != nil {
		return nil, err
	}
	mc.mu.Lock()
	if rs, ok := mc.roots[r.path]; ok {
		rs.mu.Lock()
		failed := rs.failed
		rs.mu.Unlock()
		if failed == nil {
			mc.mu.Unlock()
			return rs, nil
		}
		delete(mc.roots, r.path)
	}
	conn := mc.conn
	rs := &rootSync{mc: mc, root: r, wake: make(chan struct{}, 1), ackCh: make(chan struct{})}
	mc.roots[r.path] = rs
	mc.mu.Unlock()

	st, pc, err := transport.OpenStream(ctx, conn, mc.token, proto.KindSync)
	if err != nil {
		rs.stop(err)
		return nil, err
	}
	if err := pc.Send(proto.TMount, proto.Mount{Root: r.path, WorkerLocal: workerLocal}); err != nil {
		rs.stop(err)
		return nil, err
	}
	var ack proto.Ack
	if err := pc.Expect(proto.TAck, &ack); err != nil {
		rs.stop(err)
		return nil, err
	}
	full := r.subscribe(rs)
	rs.queue = append([]manifest.Delta{full}, rs.queue...)
	go rs.sendLoop(st, pc)
	go rs.ackLoop(pc)
	rs.kick()
	return rs, nil
}

func (rs *rootSync) sendLoop(st *quic.Stream, pc *proto.Conn) {
	defer st.Close()
	for {
		rs.mu.Lock()
		if rs.stopped {
			rs.mu.Unlock()
			return
		}
		q := rs.queue
		rs.queue = nil
		rs.mu.Unlock()
		for _, d := range q {
			if err := pc.Send(proto.TDelta, d); err != nil {
				rs.stop(err)
				return
			}
		}
		<-rs.wake
	}
}

func (rs *rootSync) ackLoop(pc *proto.Conn) {
	for {
		var ack proto.Ack
		if err := pc.Expect(proto.TAck, &ack); err != nil {
			rs.stop(err)
			return
		}
		rs.mu.Lock()
		if ack.Version > rs.acked {
			rs.acked = ack.Version
		}
		close(rs.ackCh)
		rs.ackCh = make(chan struct{})
		rs.mu.Unlock()
	}
}

// serveBlobs answers content requests from the machine.
func (mc *machineConn) serveBlobs(conn *quic.Conn) {
	for {
		st, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go mc.serveBlob(st)
	}
}

func (mc *machineConn) serveBlob(st *quic.Stream) {
	defer st.Close()
	pc := proto.NewConn(st)
	var hello proto.Hello
	if err := pc.Expect(proto.THello, &hello); err != nil || hello.Kind != proto.KindBlob {
		return
	}
	var req proto.BlobReq
	if err := pc.Expect(proto.TBlobReq, &req); err != nil {
		return
	}
	fail := func(err error) { pc.Send(proto.TBlobResp, proto.BlobResp{Err: err.Error()}) }
	r := mc.d.rootByPath(req.Root)
	if r == nil {
		fail(errors.New("unknown root"))
		return
	}
	e, ok := r.entry(req.Path)
	if !ok || e.Kind != manifest.File {
		fail(errors.New("not a projected file"))
		return
	}
	if e.Hash != req.Hash {
		fail(errors.New("file changed"))
		return
	}
	f, err := os.Open(filepath.Join(r.path, filepath.FromSlash(req.Path)))
	if err != nil {
		fail(err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(err)
		return
	}
	if err := pc.Send(proto.TBlobResp, proto.BlobResp{Size: fi.Size()}); err != nil {
		return
	}
	if err := pc.SendData(io.LimitReader(f, fi.Size())); err != nil {
		return
	}
	pc.Write(proto.TDataEnd, nil)
}
