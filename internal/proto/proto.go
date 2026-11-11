// Package proto defines the wire protocol between proxy, client daemon and
// server. Every QUIC stream carries length prefixed frames: one byte type,
// four bytes big endian length, payload. Control payloads are JSON.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/metrics"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// ALPN is the TLS application protocol of cloudsourced.
const ALPN = "cloudsourced/1"

// MaxFrame is the largest accepted frame payload.
const MaxFrame = 64 << 20

// ChunkSize is the payload size used for raw data frames.
const ChunkSize = 256 << 10

// Frame types.
const (
	THello byte = iota + 1
	TError
	TMount
	TDelta
	TAck
	TBlobReq
	TBlobResp
	TData
	TDataEnd
	TExec
	TStarted
	TStdout
	TStderr
	TStdin
	TStdinEOF
	TSignal
	TResize
	TExit
	TWriteback
	TNeed
	TBlobHeader
	TWritebackResult
	TOutputs
	TDone
	TStatus
	TFetch
	TOutList
	TOutDir
	TOutRead
	TForward
)

// Stream kinds sent in Hello.
const (
	KindSync   = "sync"
	KindBlob   = "blob"
	KindExec   = "exec"
	KindStatus = "status"
	KindFetch  = "fetch"
	// KindOutputs serves the output FUSE on the laptop: one TOutList or
	// TOutRead request per stream against the latest build slot of a repo.
	KindOutputs = "outputs"
	// KindForward carries one forwarded TCP connection into a session's
	// network namespace: TForward, then TAck (or TError), then raw bytes
	// in both directions until either side closes.
	KindForward = "forward"
)

// Hello opens every stream.
type Hello struct {
	Token string `json:"token,omitempty"`
	Kind  string `json:"kind"`
	// TraceParent is the W3C trace context of the opening side (optional;
	// older peers omit and ignore it).
	TraceParent string `json:"traceparent,omitempty"`
}

// Error reports a failure. Stage "prepare" means nothing ran remotely yet.
type Error struct {
	Stage string `json:"stage,omitempty"`
	Msg   string `json:"msg"`
}

func (e *Error) Error() string { return e.Stage + ": " + e.Msg }

// Mount asks the server to project a root.
type Mount struct {
	Root string `json:"root"`
	// Hidden lists worker-local directories (relative) that are not part of
	// the projection but may exist as mount points.
	WorkerLocal []string `json:"worker_local,omitempty"`
}

// Ack confirms that a manifest version has been applied.
type Ack struct {
	Version uint64 `json:"v"`
}

// BlobReq requests a file's content from the client.
type BlobReq struct {
	Root string `json:"root"`
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// BlobResp precedes blob data.
type BlobResp struct {
	Size int64  `json:"size"`
	Err  string `json:"err,omitempty"`
}

// TTY describes the local terminal.
type TTY struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	Term string `json:"term,omitempty"`
}

// Exec starts a remote process.
type Exec struct {
	Repo   string            `json:"repo"` // primary root
	RepoID string            `json:"repo_id"`
	Roots  map[string]uint64 `json:"roots"` // root -> minimum manifest version (barrier)
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd"`
	Env    map[string]string `json:"env"`
	TTY    *TTY              `json:"tty,omitempty"`
	// Grant holds the reservation and limits decided by the scheduler.
	Grant scheduler.Grant `json:"grant"`
	// MemoryOvercommit bounds the sum of memory reservations on the
	// machine (server side safety net for the scheduler).
	MemoryOvercommit float64  `json:"memory_overcommit"`
	Build            []string `json:"build"`     // worker-local dirs relative to Repo
	BuildTTL         string   `json:"build_ttl"` // e.g. "24h"
	Deps             []string `json:"deps"`      // shared dependency cache paths
	Eager            []string `json:"eager"`     // output globs relative to Repo
	Lease            string   `json:"lease,omitempty"`
	// CompilerCache enables a compiler cache in the session (repository
	// [cache.compiler]); nil means off.
	CompilerCache *CompilerCache `json:"compiler_cache,omitempty"`
	// Linker is the repository's environment.linker ("", "mold", "lld").
	Linker string `json:"linker,omitempty"`
	// Toolchain relocates the image's Rust toolchain to the client's
	// paths ([outputs] mode = "fuse"), so that locally running cargo
	// considers the remote build directory fresh; nil keeps the image's.
	Toolchain *Toolchain `json:"toolchain,omitempty"`
	// TraceParent is the W3C trace context of the tool run (optional);
	// the server's session span continues it.
	TraceParent string `json:"traceparent,omitempty"`
}

// CompilerCache is the session's compiler cache configuration.
type CompilerCache struct {
	Tool string `json:"tool"` // "sccache"
	// TTLSeconds lowers the maximum age of reused entries (0 = cache limit).
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
	// Incremental false sets CARGO_INCREMENTAL=0; nil keeps cargo's default.
	Incremental *bool `json:"incremental,omitempty"`
}

// Toolchain holds the client's toolchain locations. Cargo hashes the
// absolute paths of registry sources ($CARGO_HOME/registry/src/...) and the
// configured linker into its fingerprints.
type Toolchain struct {
	CargoHome  string `json:"cargo_home"`  // absolute
	RustupHome string `json:"rustup_home"` // absolute
}

// Started confirms the remote process runs.
type Started struct {
	Session string `json:"session"`
	Machine string `json:"machine"`
	Slot    string `json:"slot"`
	WaitMs  int64  `json:"wait_ms"` // barrier and preparation time
}

// Signal forwards a signal.
type Signal struct {
	Sig int `json:"sig"`
}

// Resize forwards a terminal resize.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Exit reports process termination.
type Exit struct {
	Code   int             `json:"code"`
	Signal int             `json:"signal,omitempty"`
	Usage  scheduler.Usage `json:"usage"`
	// Cache holds counters of cache integrations (e.g. "sccache_hits",
	// "sccache_misses"); shown by `cloudsourced why`.
	Cache map[string]int64 `json:"cache,omitempty"`
}

// Forward asks the server to connect to 127.0.0.1:Port in the network
// namespace of a running session.
type Forward struct {
	Session string `json:"session"`
	Port    int    `json:"port"`
}

// Change is a file written remotely into a projected root.
type Change struct {
	Root     string        `json:"root"`
	Path     string        `json:"path"`
	Deleted  bool          `json:"deleted,omitempty"`
	Kind     manifest.Kind `json:"kind,omitempty"`
	Mode     uint32        `json:"mode,omitempty"`
	Hash     string        `json:"hash,omitempty"`
	Size     int64         `json:"size,omitempty"`
	Target   string        `json:"target,omitempty"`
	BaseHash string        `json:"base,omitempty"` // content the change was based on ("" = did not exist)
}

// Writeback lists remote changes to projected roots.
type Writeback struct {
	Changes []Change `json:"changes"`
}

// Need requests blobs by hash.
type Need struct {
	Hashes []string `json:"hashes"`
}

// BlobHeader precedes TData frames of one blob, terminated by TDataEnd.
type BlobHeader struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// WritebackResult reports which changes were not applied locally.
type WritebackResult struct {
	Rejected []string `json:"rejected,omitempty"` // "root\x00path"
}

// Outputs lists output files available for transfer.
type Outputs struct {
	Files []manifest.Entry `json:"files"` // paths relative to Exec.Repo
}

// Status describes a server.
type Status struct {
	Machine    string  `json:"machine"`
	EnvHash    string  `json:"env_hash"`
	Ready      bool    `json:"ready"`
	CPU        float64 `json:"cpu"`
	Memory     int64   `json:"memory"`
	UsedCPU    float64 `json:"used_cpu"`    // sum of session reservations
	UsedMemory int64   `json:"used_memory"` // sum of session reservations
	// Measured is the actual usage of all sessions.
	Measured  scheduler.Resources `json:"measured"`
	Pressure  scheduler.Pressure  `json:"pressure"`
	Sessions  []Session           `json:"sessions"`
	IdleSince int64               `json:"idle_since"` // unix seconds, 0 while busy
	ExpiresAt int64               `json:"expires_at"`
	Mounts    []string            `json:"mounts"`
	Draining  bool                `json:"draining"`
	// InterruptAt is when the machine will be taken away (unix seconds; 0:
	// no notice), e.g. a spot interruption: the server drains and running
	// sessions get until then.
	InterruptAt int64 `json:"interrupt_at,omitempty"`
	// DrainReason says why a server drains on its own ("expired",
	// "spot-interruption"; empty: by the orchestrator).
	DrainReason string `json:"drain_reason,omitempty"`
	// AtRisk is set after a rebalance recommendation (spot): the machine
	// keeps serving, but an interruption is more likely.
	AtRisk    bool     `json:"at_risk,omitempty"`
	WarmRepos []string `json:"warm_repos,omitempty"` // repo IDs with unexpired build slots
	// Metrics is the server's metrics snapshot; the orchestrator republishes
	// it with machine labels (no extra port or scope on the machine).
	Metrics []metrics.Family `json:"metrics,omitempty"`
}

// Session is a running remote process.
type Session struct {
	ID      string   `json:"id"`
	Repo    string   `json:"repo"`
	Argv    []string `json:"argv"`
	Started int64    `json:"started"`
	CPU     float64  `json:"cpu"`
	Memory  int64    `json:"memory"`
	Lease   string   `json:"lease,omitempty"`
}

// Fetch requests output files from the most recent build slot of a repo.
type Fetch struct {
	RepoID string   `json:"repo_id"`
	Repo   string   `json:"repo"`
	Paths  []string `json:"paths"` // relative to repo, globs allowed
}

// OutList requests the entries of one directory inside the worker-local
// build directories of the latest build slot. Dir is relative to the
// repository ("target/debug"); "" lists the build directories themselves.
// The answer is TOutDir.
type OutList struct {
	RepoID string `json:"repo_id"`
	Dir    string `json:"dir"`
}

// OutEntry is one directory entry of the build slot. Content hashes are not
// listed (hashing a target directory is expensive); the laptop identifies
// content by path, size and mtime and hashes what it downloads.
type OutEntry struct {
	Name    string        `json:"n"`
	Kind    manifest.Kind `json:"k"`
	Mode    uint32        `json:"m,omitempty"`
	Size    int64         `json:"s,omitempty"`
	MtimeNs int64         `json:"t,omitempty"`
	Target  string        `json:"l,omitempty"` // symlink target
}

// OutDir answers OutList.
type OutDir struct {
	Slot    string     `json:"slot"`
	Entries []OutEntry `json:"entries"`
}

// OutRead requests a whole file of the latest build slot. Size and MtimeNs
// are those of the listing the client saw; if the file changed since, the
// server answers TError (stage "stale"). Otherwise it sends TBlobHeader
// (hash empty, size), TData frames and TDataEnd. A change during transfer
// replaces TDataEnd with TError.
type OutRead struct {
	RepoID  string `json:"repo_id"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

// Conn frames a bidirectional stream. Writes are safe for concurrent use.
type Conn struct {
	rw  io.ReadWriter
	wmu sync.Mutex
	hdr [5]byte
}

// NewConn wraps a stream.
func NewConn(rw io.ReadWriter) *Conn { return &Conn{rw: rw} }

// Write sends one frame.
func (c *Conn) Write(t byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("proto: frame too large (%d)", len(payload))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.rw.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Send writes a JSON control frame.
func (c *Conn) Send(t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(t, b)
}

// SendError writes an error frame.
func (c *Conn) SendError(stage string, err error) error {
	return c.Send(TError, Error{Stage: stage, Msg: err.Error()})
}

// Read reads one frame. It is not safe for concurrent use.
func (c *Conn) Read() (byte, []byte, error) {
	if _, err := io.ReadFull(c.rw, c.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(c.hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("proto: frame too large (%d)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.rw, buf); err != nil {
		return 0, nil, err
	}
	return c.hdr[0], buf, nil
}

// Expect reads a frame of type t and decodes it into v. An error frame is
// returned as *Error.
func (c *Conn) Expect(t byte, v any) error {
	got, payload, err := c.Read()
	if err != nil {
		return err
	}
	return Decode(got, payload, t, v)
}

// Decode checks the frame type and decodes payload.
func Decode(got byte, payload []byte, want byte, v any) error {
	if got == TError {
		var e Error
		if err := json.Unmarshal(payload, &e); err != nil {
			return fmt.Errorf("proto: bad error frame: %w", err)
		}
		return &e
	}
	if got != want {
		return fmt.Errorf("proto: unexpected frame type %d (want %d)", got, want)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(payload, v)
}

// SendBlob streams r as TBlobHeader, TData... and TDataEnd.
func (c *Conn) SendBlob(hash string, size int64, r io.Reader) error {
	if err := c.Send(TBlobHeader, BlobHeader{Hash: hash, Size: size}); err != nil {
		return err
	}
	if err := c.SendData(r); err != nil {
		return err
	}
	return c.Write(TDataEnd, nil)
}

// SendData streams r as TData frames (without terminator).
func (c *Conn) SendData(r io.Reader) error {
	buf := make([]byte, ChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := c.Write(TData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ReadData copies TData frames into w until TDataEnd.
func (c *Conn) ReadData(w io.Writer) error {
	for {
		t, p, err := c.Read()
		if err != nil {
			return err
		}
		switch t {
		case TData:
			if _, err := w.Write(p); err != nil {
				return err
			}
		case TDataEnd:
			return nil
		default:
			return Decode(t, p, TData, nil)
		}
	}
}
