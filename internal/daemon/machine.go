package daemon

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
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/transport"
)

// machineConn is the daemon's long-lived connection to one machine. It
// carries one sync stream per attached root and serves blob requests.
type machineConn struct {
	id, addr, fingerprint string
	d                     *Daemon
	log                   *slog.Logger

	mu    sync.Mutex
	token string // session token of the newest lease on this machine
	conn  *quic.Conn
	roots map[string]*rootSync
	// closed: the connection was closed on purpose (no reconnect).
	closed bool
}

// rootSync streams one root's manifest to one machine.
type rootSync struct {
	mc          *machineConn
	root        *root
	workerLocal []string

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
	// Sync streams use the newest lease's session token (tokens are bound
	// to a lease; an old one would outlive its lease's purpose).
	mc.mu.Lock()
	mc.token = token
	mc.mu.Unlock()
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
	go mc.watch(conn)
	return nil
}

// reconnectWindow bounds how long a lost machine connection is redialed:
// a session on the machine blocks its source reads for a while (the
// worker's source wait) and resumes once the roots are attached again.
var reconnectWindow = 2 * time.Minute

// watch re-establishes the connection and re-attaches the roots that were
// attached when conn broke (a network interruption): file faults of
// running sessions wait for the source instead of failing.
func (mc *machineConn) watch(conn *quic.Conn) {
	<-conn.Context().Done()
	mc.mu.Lock()
	if mc.closed {
		mc.mu.Unlock()
		return
	}
	type att struct {
		r  *root
		wl []string
	}
	var roots []att
	for _, rs := range mc.roots {
		roots = append(roots, att{rs.root, rs.workerLocal})
	}
	mc.mu.Unlock()
	if len(roots) == 0 {
		return
	}
	cause := context.Cause(conn.Context())
	mc.log.Warn("machine connection lost, reconnecting", "machine", mc.id, "err", cause)
	deadline := time.Now().Add(reconnectWindow)
	backoff := 500 * time.Millisecond
	for time.Now().Before(deadline) {
		mc.mu.Lock()
		stop := mc.closed
		mc.mu.Unlock()
		if stop {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var err error
		for _, a := range roots {
			if _, err = mc.attach(ctx, a.r, a.wl); err != nil {
				break
			}
		}
		cancel()
		if err == nil {
			mc.log.Info("machine connection restored", "machine", mc.id, "roots", len(roots))
			return
		}
		mc.log.Debug("reconnect failed", "machine", mc.id, "err", err)
		time.Sleep(backoff)
		backoff = min(backoff*2, 10*time.Second)
	}
	mc.log.Warn("machine connection not restored", "machine", mc.id, "window", reconnectWindow)
}

func (mc *machineConn) close() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.closed = true
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
	conn, token := mc.conn, mc.token
	rs := &rootSync{mc: mc, root: r, workerLocal: workerLocal, wake: make(chan struct{}, 1), ackCh: make(chan struct{})}
	mc.roots[r.path] = rs
	mc.mu.Unlock()

	st, pc, err := transport.OpenStream(ctx, conn, token, proto.KindSync)
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
