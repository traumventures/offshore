// Package orchestrator assigns sessions to machines and manages the pool:
// cache-affine scheduling, scale up, warm pool, idle stop, lifetime
// rotation and cleanup of expired resources.
package orchestrator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/traumventures/offshore/internal/clock"
	"github.com/traumventures/offshore/internal/learning"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/logs"
	"github.com/traumventures/offshore/internal/medium"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/store"
	"github.com/traumventures/offshore/internal/tracing"
	"github.com/traumventures/offshore/internal/transport"
)

// Config is the pool configuration. Placement and scaling decisions are
// made by Policy; the orchestrator only gathers state and executes them.
// It holds pool settings only: repository settings travel in Request.
type Config struct {
	Owner        string           // pool id, the offshore.pool label of all resources
	Sizes        []scheduler.Size // machine types; the first is the default
	MaxLifetime  time.Duration
	QueueTimeout time.Duration
	Provision    []string
	Swap         bool // machines allow sessions to swap
	Policy       scheduler.Config
	// EventRetention bounds the analysis event log (0 = 30 days).
	EventRetention time.Duration
	// DirtyRatio and DirtyExpire are the workers' build slot writeback
	// thresholds ([writeback]; zero: the workers' defaults).
	DirtyRatio  float64
	DirtyExpire time.Duration
	// SourceCache is [scheduler] source_cache, passed to the workers
	// ("": their default).
	SourceCache string
	// Profile resolves a provider's medium profile (instance-store sizes
	// get medium.LocalNVMe once their disk is known; nil: defaults).
	Profile func(provider, med string) medium.Profile `json:"-"`
}

// Default repository cache lifetimes of requests that carry none (older
// clients).
const (
	DefaultBuildTTL = 24 * time.Hour
	DefaultDepsTTL  = 7 * 24 * time.Hour
)

// Request asks for capacity for one session.
type Request struct {
	EnvHash  string
	Image    string
	Services []provider.Service
	RepoID   string
	Key      string              // learning key (repo/tool/subcommand)
	Need     scheduler.Resources // estimated need
	// Estimate is where Need came from (estimator key, source, runs,
	// statistic); recorded on task_placed (nil: unknown).
	Estimate *scheduler.Estimate `json:",omitempty"`
	Limit    scheduler.Resources // hard ceiling (0 = none)
	// BuildTTL bounds the age of the session's build slot data
	// (shortest slot-scope cache ttl; 0 = DefaultBuildTTL). The effective value, capped
	// by max_lifetime, is returned in Lease.BuildTTL and enforced per slot
	// by the worker, so repositories with different TTLs can share
	// a machine.
	BuildTTL time.Duration `json:",omitempty"`
	// DepsTTL is the lifetime of the shared dependency cache volume a new
	// machine created for this request starts (longest machine-scope cache ttl; 0 =
	// DefaultDepsTTL). Existing machines keep the volume they have.
	DepsTTL time.Duration `json:",omitempty"`
	// Arch is the machine architecture the environment image is built for
	// (resources.arch: "x86_64"/"amd64" or "aarch64"/"arm64"; empty:
	// amd64). Machines for the environment are only created from sizes of
	// this architecture.
	Arch string `json:",omitempty"`
	// Labels describe who runs the session (worktree, branch, user, agent
	// and free labels, see internal/labels). They are recorded in the
	// event log and shown in the queue; they never affect placement.
	Labels map[string]string `json:",omitempty"`
	// Family identifies the repository across its worktrees (empty:
	// unknown, e.g. not a git repository): a worktree without a head is
	// seeded from a sibling's head (scheduler.Task.Family).
	Family string `json:",omitempty"`
}

// QueuedTask is a request waiting for capacity (`offshore top`).
type QueuedTask struct {
	ID     string              `json:"id"`
	Since  time.Time           `json:"since"`
	RepoID string              `json:"repo_id,omitempty"`
	Key    string              `json:"key,omitempty"`
	Env    string              `json:"env,omitempty"` // environment hash
	Need   scheduler.Resources `json:"need"`
	Labels map[string]string   `json:"labels,omitempty"`
	// Waiting is what the task waits for (updated every placement round).
	Waiting *events.Waiting `json:"waiting,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	// Queue is the queue-confidence verdict of its last placement
	// ("wait: p=0.86", "create now: p=0.41").
	Queue string `json:"queue,omitempty"`
}

// buildTTL returns the effective build TTL of a request.
func (r Request) buildTTL(maxLifetime time.Duration) time.Duration {
	ttl := r.BuildTTL
	if ttl <= 0 {
		ttl = DefaultBuildTTL
	}
	// The build cache never outlives a machine, and machines never outlive
	// max_lifetime.
	if maxLifetime > 0 && ttl > maxLifetime {
		ttl = maxLifetime
	}
	return ttl
}

// repoParams are the machine parameters derived from a request's
// repository, applied when a machine is created for it.
type repoParams struct {
	DepsTTL time.Duration `json:"deps_ttl,omitempty"`
	Arch    string        `json:"arch,omitempty"` // machine architecture (amd64, arm64)
}

// machineArch normalizes an architecture name to the size naming
// ("amd64", "arm64").
func machineArch(a string) string {
	switch a {
	case "", "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return a
}

func (r Request) params() repoParams {
	p := repoParams{DepsTTL: r.DepsTTL, Arch: machineArch(r.Arch)}
	if p.DepsTTL <= 0 {
		p.DepsTTL = DefaultDepsTTL
	}
	return p
}

// Lease is a reservation on a machine.
type Lease struct {
	ID          string
	MachineID   string
	Addr        string
	Fingerprint string
	// Token is a session token signed by the control plane, valid only for
	// this lease on this machine until shortly after the machine expires.
	Token string
	Grant scheduler.Grant
	// MemoryOvercommit is the pool's memory overcommit factor; the server
	// uses it as admission safety net.
	MemoryOvercommit float64
	// Provider, Size and PricePerHour describe the leased machine; Reason
	// is the policy's placement reason (shown by `offshore why`).
	Provider     string
	Size         string
	PricePerHour float64
	Reason       string
	// BuildTTL is the effective build slot TTL of the session (the
	// request's, capped by max_lifetime).
	BuildTTL time.Duration `json:",omitempty"`
	// Store grants the session's worker access to the repository's and
	// environment's prefixes of the machine's store (nil: no store).
	Store *store.Grant `json:",omitempty"`
	// Decision is the placement decision (event log only; also set on
	// failed acquires: the last decision before the timeout or reject).
	Decision *scheduler.Decision `json:"-"`
	// Predicted is what the policy expected of the run (event log only).
	Predicted *events.Predicted `json:"-"`
}

// Machine is the orchestrator view of a machine.
type Machine struct {
	provider.Machine
	Provider string // name of the provider instance
	Status   *proto.Status
	StatusAt time.Time
	// LastReady is when the worker last reported ready (pool clock; zero:
	// never seen ready).
	LastReady time.Time
	// sampledAt is when the last machine_sample event was written.
	sampledAt time.Time
	// thrashing is the last reported thrash state (events on change).
	thrashing bool
	// sampledVM is the cache file system state of the last sample (the
	// base of the page_fault, page_in and reclaim deltas).
	sampledVM *proto.VMStats
	Draining  bool
	// PricePerHour comes from the pool size of the machine (0: unknown).
	PricePerHour float64
	// Uptime lists the periods in which the machine was observed starting
	// or running (the last one is open while it is up). It is kept in
	// memory for about two days. A machine first seen up counts from its
	// CreatedAt, so restarts before the orchestrator started are not
	// visible; see UpTime.
	Uptime []Span
	// Removed is when a destroyed machine disappeared; such machines are
	// listed for cost accounting only.
	Removed  time.Time
	observed bool
	seenAt   time.Time // first observed by this orchestrator (pool clock)
	price    float64   // hourly price recorded at creation or in its events (0: unknown)
	pending  map[string]pendingLease
	// stopping: a stop or destroy passed its last check and is under way;
	// the machine is not undrained any more.
	stopping bool
	// undrains counts undrain decisions (a stop that began before one is
	// cancelled).
	undrains int
	// rebootedAt is when the orchestrator last rebooted the machine
	// because it was unreachable (zero: never).
	rebootedAt time.Time
	// listed is when the provider view in Machine was taken (the start of
	// the listing) or last changed locally. Older listings are stale: a
	// slow readiness poll must not overwrite a newer state or expiry.
	listed time.Time
}

// UnhealthyAfter is how long a machine running at its provider may go
// without a ready worker (booting, or after it was ready) before it is
// replaced.
const UnhealthyAfter = 10 * time.Minute

// UnreachableAfter is how long a machine whose worker was ready may go
// without a successful probe before it is unreachable: it stops counting
// as capacity for queued requests, and while requests wait for its
// environment it is rebooted, then replaced (RebootGrace).
const UnreachableAfter = 2 * time.Minute

// RebootGrace is how long an unreachable machine gets after a reboot to
// answer again before it is replaced.
const RebootGrace = 3 * time.Minute

// UnreachableFor is how long a machine whose worker was ready in its
// current uptime period has not answered a probe (0: reachable, booting
// or not running).
func (m *Machine) UnreachableFor(now time.Time) time.Duration {
	if !m.EverReady() {
		return 0
	}
	return m.NotReadyFor(now)
}

// Unreachable reports whether m is unreachable (UnreachableAfter).
func (m *Machine) Unreachable(now time.Time) bool {
	return m.UnreachableFor(now) > UnreachableAfter
}

// UpSince is the start of the machine's current uptime period (zero:
// not up).
func (m *Machine) UpSince() time.Time {
	if n := len(m.Uptime); n > 0 && m.Uptime[n-1].End.IsZero() {
		return m.Uptime[n-1].Start
	}
	return time.Time{}
}

// EverReady reports whether the worker was ready in the current uptime
// period (a machine without one: ever).
func (m *Machine) EverReady() bool {
	return !m.LastReady.IsZero() && !m.LastReady.Before(m.UpSince())
}

// NotReadyFor is how long a machine running at its provider has been
// without a ready worker: since its uptime period began when it was not
// ready in it yet (booting), else since it last reported ready (0: ready,
// or not running).
func (m *Machine) NotReadyFor(now time.Time) time.Duration {
	if m.State != provider.Running || !m.Removed.IsZero() || m.Status != nil && m.Status.Ready {
		return 0
	}
	since := m.UpSince()
	if m.EverReady() {
		since = m.LastReady
	}
	if since.IsZero() {
		return 0
	}
	// Not before this orchestrator saw it (after a restart nothing is
	// known about earlier readiness).
	if since.Before(m.seenAt) {
		since = m.seenAt
	}
	return now.Sub(since)
}

// readyLocked records a ready status of m.
func (o *Orchestrator) readyLocked(m *Machine) {
	if m.Status != nil && m.Status.Ready {
		m.LastReady = o.now()
	}
}

// replaceable reports whether the orchestrator may replace an unhealthy
// machine (a static host is the user's, not the pool's).
func (o *Orchestrator) replaceable(m *Machine) bool {
	p, ok := o.provs[m.Provider]
	return ok && p.Name() != "static"
}

// update applies a provider listing started at listedAt unless the machine
// has a newer view; it reports whether pm was applied.
func (m *Machine) update(pm provider.Machine, listedAt time.Time) bool {
	if listedAt.Before(m.listed) {
		return false
	}
	m.Machine, m.listed = pm, listedAt
	return true
}

// Span is a period of machine uptime. End is zero while the period lasts.
type Span struct {
	Start time.Time
	End   time.Time
}

// historyKeep bounds the uptime history of machines.
const historyKeep = 48 * time.Hour

// observe records state transitions of the machine into its uptime.
func (m *Machine) observe(now time.Time) {
	up := m.State == provider.Running || m.State == provider.Starting
	open := len(m.Uptime) > 0 && m.Uptime[len(m.Uptime)-1].End.IsZero()
	switch {
	case up && !open:
		start := now
		if !m.observed && !m.CreatedAt.IsZero() && m.CreatedAt.Before(now) {
			start = m.CreatedAt
		}
		m.Uptime = append(m.Uptime, Span{Start: start})
	case !up && open:
		m.Uptime[len(m.Uptime)-1].End = now
	}
	if !m.observed {
		m.seenAt = now
	}
	m.observed = true
	for len(m.Uptime) > 0 && !m.Uptime[0].End.IsZero() && now.Sub(m.Uptime[0].End) > historyKeep {
		m.Uptime = m.Uptime[1:]
	}
}

// UpTime returns how long the machine was up between from and now.
func (m *Machine) UpTime(from, now time.Time) time.Duration {
	var d time.Duration
	for _, sp := range m.Uptime {
		start, end := sp.Start, sp.End
		if end.IsZero() || end.After(now) {
			end = now
		}
		if start.Before(from) {
			start = from
		}
		if end.After(start) {
			d += end.Sub(start)
		}
	}
	return d
}

// Cost returns the estimated cost of the machine's uptime since from.
func (m *Machine) Cost(from, now time.Time) float64 {
	return m.PricePerHour * m.UpTime(from, now).Hours()
}

type pendingLease struct {
	res     scheduler.Resources
	expires time.Time
}

// creation is a machine being created; it is capacity on its way.
type creation struct {
	prov    string
	env     string
	size    scheduler.Size
	at      time.Time
	started bool // start of a stopped machine (else create)
	// provided: the provider's create or start call returned.
	provided time.Time
	volume   string // detached cache volume the create attaches (policy choice)
}

// Orchestrator manages one owner's pool.
type Orchestrator struct {
	cfg      Config
	provs    map[string]provider.Provider // by provider instance name
	stateDir string
	log      *slog.Logger
	master   auth.Master
	signer   ed25519.PrivateKey

	mu         sync.Mutex
	policy     scheduler.Policy
	machines   map[string]*Machine
	refreshed  time.Time
	creating   map[string]creation       // machine id -> creation
	queue      map[string]scheduler.Task // waiting tasks
	queued     map[string]repoParams     // task id -> repository parameters
	waiting    map[string]QueuedTask     // task id -> request shown in Queue
	envParams  map[string]repoParams     // env hash -> parameters of its last request
	recentEnvs map[string]time.Time      // env hash -> last use (warm pool)
	images     map[string]string         // env hash -> image
	removed    []*Machine                // destroyed machines (cost history)
	seedUptime map[string]*Machine       // uptime replayed at start, merged on first listing
	services   map[string][]provider.Service
	changed    chan struct{}
	// meter measures medium profiles from the workers' tier counters
	// (installed with medium.SetMeasured).
	meter *medium.Meter

	// OnMachineDown is called when a machine stops or disappears, so that
	// connections to it can be dropped.
	OnMachineDown func(id string)

	// ProbeFunc fetches a server status; replaceable in tests.
	ProbeFunc func(ctx context.Context, addr, fingerprint, token string) (proto.Status, error)

	// Clock is the time of pool decisions: idle stop, lifetime expiry,
	// pending lease and volume TTLs, warm pool windows, uptime. Waits,
	// probe recency and latency metrics use real time. Set before use;
	// New takes it from $OFFSHORE_TEST_CLOCK (default: real time).
	Clock clock.Clock

	// PrepareImage makes the environment image available on a provider
	// right before a machine of the given size is created there and returns
	// the reference the machine runs (nil, or an empty reference: the image
	// is assumed present under the environment's tag).
	PrepareImage func(ctx context.Context, provName string, prov provider.Provider, envHash, size string) (string, error)

	// Profile returns what the estimator learned about a learning key
	// (durations, slot sizes) for the cost model; nil = unknown.
	Profile func(key string) scheduler.Profile

	health         map[string]provHealth  // provider instance -> health and latencies
	leases         map[string]leaseInfo   // lease id -> expected session end
	heads          map[string]*repoLayers // build slot heads and layers (layers.go)
	families       map[string][]string    // repository ids per family (worktrees of a repository, families.go)
	layersGC       time.Time
	volumes        map[string]*VolumeState // managed cache volumes by id
	localDisksOnce sync.Once
	startBlocked   map[string]time.Time     // machine -> no start before
	createFail     map[string]createBackoff // env/provider -> create backoff
	downing        map[string]bool          // machines with a stop or destroy running

	// Slots performs build slot operations on workers (prune, snapshot
	// save and restore); nil disables slot moves and prunes.
	Slots SlotOps
	// Stores mints worker store grants per provider (nil: no stores).
	Stores *StoreSet

	metrics *orchMetrics
	obs     *observer
	learn   *learning.Store // what the scheduler learns from (never the event log)
	// arrivals are the request arrivals per environment (learning store):
	// the lifecycle policy keeps, stops or destroys idle machines by them.
	arrivals *learning.Arrivals
}

// New creates an orchestrator.
// New creates an orchestrator over one or more named providers. One
// orchestrator may span several providers (e.g. local Docker plus AWS); the
// policy chooses among their sizes.
func New(cfg Config, provs map[string]provider.Provider, stateDir string, log *slog.Logger) (*Orchestrator, error) {
	if len(provs) == 0 {
		return nil, errors.New("orchestrator: no providers")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	master, err := auth.LoadMaster(stateDir)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: pool master key: %w", err)
	}
	fixConfig(&cfg)
	clk, err := clock.FromEnv()
	if err != nil {
		return nil, err
	}
	o := &Orchestrator{
		cfg: cfg, provs: provs, stateDir: stateDir, log: log, master: master, signer: master.Signer(),
		policy:   scheduler.Default{Cfg: cfg.Policy},
		machines: map[string]*Machine{}, creating: map[string]creation{}, queue: map[string]scheduler.Task{},
		queued: map[string]repoParams{}, waiting: map[string]QueuedTask{}, envParams: map[string]repoParams{},
		recentEnvs: map[string]time.Time{}, images: map[string]string{}, services: map[string][]provider.Service{}, changed: make(chan struct{}),
		ProbeFunc: Probe, Clock: clk,
		health: map[string]provHealth{}, leases: map[string]leaseInfo{}, heads: map[string]*repoLayers{},
		volumes: map[string]*VolumeState{}, startBlocked: map[string]time.Time{}, createFail: map[string]createBackoff{}, downing: map[string]bool{},
		meter: medium.NewMeter(),
	}
	medium.SetMeasured(o.meter.Profile)
	o.loadHeads()
	o.loadFamilies()
	o.loadVolumes()
	o.metrics = newOrchMetrics(o)
	o.learn = learning.Open(stateDir)
	o.arrivals = o.learn.Arrivals()
	o.obs = newObserver(stateDir, cfg.EventRetention, o.learn)
	o.obs.log.SetWarn(func(msg string, err error) { log.Warn(msg, "err", err) })
	o.loadUptime()
	o.loadRecent()
	return o, nil
}

// SetConfig updates the pool configuration and rebuilds the default policy.
// It is meant for explicit reloads of the pool configuration, never for
// request specific settings (those travel in Request).
func (o *Orchestrator) SetConfig(cfg Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fixConfig(&cfg)
	o.obs.log.SetRetention(cfg.EventRetention)
	if _, ok := o.policy.(scheduler.Default); ok {
		o.policy = scheduler.Default{Cfg: cfg.Policy}
	}
	o.cfg = cfg
	o.localDisksOnce = sync.Once{} // sizes are new: resolve their disks again
	for name := range o.provs {    // a configuration change may fix credentials or the account
		o.resetProviderLocked(name, "configuration reloaded")
	}
}

// Config returns the pool configuration in effect.
func (o *Orchestrator) Config() Config {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg
}

// SetPolicy replaces the scheduling policy.
func (o *Orchestrator) SetPolicy(p scheduler.Policy) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.policy = p
}

func fixConfig(cfg *Config) {
	if cfg.Policy.ExpiryMargin == 0 {
		cfg.Policy.ExpiryMargin = 15 * time.Minute
		if cfg.MaxLifetime > 0 && cfg.MaxLifetime < time.Hour {
			cfg.Policy.ExpiryMargin = cfg.MaxLifetime / 4
		}
	}
}

// now is the decision time (see Clock).
func (o *Orchestrator) now() time.Time { return o.Clock.Now() }

func (o *Orchestrator) notify() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// fingerprint returns the certificate fingerprint of a machine, recomputed
// from the master key so that provider labels need not be trusted. Machines
// without an expiry fall back to their label.
func (o *Orchestrator) fingerprint(m provider.Machine) string {
	if m.ExpiresAt.IsZero() {
		return m.Fingerprint
	}
	fp, err := o.master.MachineFingerprint(m.ID, m.ExpiresAt)
	if err != nil {
		return m.Fingerprint
	}
	if m.Fingerprint != "" && m.Fingerprint != fp {
		o.log.Debug("machine fingerprint label differs from derived identity", "id", m.ID)
	}
	return fp
}

// probeToken mints a short-lived token that only reads a machine's status.
func (o *Orchestrator) probeToken(machineID string) string {
	return auth.NewToken(o.signer, auth.ScopeProbe, machineID, "", time.Now().Add(2*time.Minute))
}

// sessionToken mints the token of a lease. It lives as long as the machine
// (builds may be long) but is bound to the lease.
func (o *Orchestrator) sessionToken(m provider.Machine, leaseID string) string {
	until := m.ExpiresAt.Add(time.Hour)
	if m.ExpiresAt.IsZero() {
		until = time.Now().Add(24 * time.Hour)
	}
	return auth.NewToken(o.signer, auth.ScopeSession, m.ID, leaseID, until)
}

// AdminToken mints a token with every permission on a machine (operator
// use; the orchestrator itself never needs one).
func (o *Orchestrator) AdminToken(machineID string, ttl time.Duration) string {
	return auth.NewToken(o.signer, auth.ScopeAdmin, machineID, "", time.Now().Add(ttl))
}

func (o *Orchestrator) recentPath() string { return filepath.Join(o.stateDir, "recent-envs.json") }

type recentEnv struct {
	Image    string             `json:"image"`
	Used     int64              `json:"used"`
	Services []provider.Service `json:"services,omitempty"`
	Params   *repoParams        `json:"params,omitempty"`
}

func (o *Orchestrator) loadRecent() {
	b, err := os.ReadFile(o.recentPath())
	if err != nil {
		return
	}
	var m map[string]recentEnv
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for h, r := range m {
		o.recentEnvs[h] = time.Unix(r.Used, 0)
		o.images[h] = r.Image
		o.services[h] = r.Services
		if r.Params != nil {
			o.envParams[h] = *r.Params
		}
	}
}

func (o *Orchestrator) saveRecentLocked() {
	m := map[string]recentEnv{}
	for h, t := range o.recentEnvs {
		re := recentEnv{Image: o.images[h], Used: t.Unix(), Services: o.services[h]}
		if p, ok := o.envParams[h]; ok {
			re.Params = &p
		}
		m[h] = re
	}
	b, _ := json.Marshal(m)
	os.WriteFile(o.recentPath(), b, 0o600)
}

func newID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Refresh reloads machines from the provider and probes running servers.
func (o *Orchestrator) Refresh(ctx context.Context) error {
	listedAt := time.Now()
	list, err := o.listAll(ctx)
	if err != nil {
		return err
	}
	go o.GCLayers(context.WithoutCancel(ctx), false)
	type probe struct {
		id     string
		status *proto.Status
	}
	results := make(chan probe, len(list))
	var wg sync.WaitGroup
	o.mu.Lock()
	seen := map[string]bool{}
	var down []string
	for _, tm := range list {
		pm := tm.Machine
		seen[pm.ID] = true
		m, ok := o.machines[pm.ID]
		if !ok {
			m = &Machine{Provider: tm.prov, pending: map[string]pendingLease{}}
			o.mergeUptimeLocked(m, pm.ID, pm.State)
			o.machines[pm.ID] = m
		}
		if listedAt.Before(m.listed) {
			continue // changed since the listing started
		}
		if m.State == provider.Running && pm.State != provider.Running {
			down = append(down, pm.ID)
		}
		expired := !pm.ExpiresAt.IsZero() && !o.now().Before(pm.ExpiresAt)
		if _, inFlight := o.creating[pm.ID]; inFlight && pm.State == provider.Stopped && !expired {
			// A start is in progress but the provider does not show it yet
			// (e.g. the container or instance is still stopped): keep the
			// machine starting, or the policy would start it again. Expired
			// machines stay stopped, so that they are destroyed.
			pm.State = provider.Starting
		}
		pm.Fingerprint = o.fingerprint(pm)
		if !ok || m.State != pm.State {
			o.stateEventLocked(pm.ID, pm.State)
		}
		m.update(pm, listedAt)
		m.observe(o.now())
		if pm.State != provider.Running {
			m.Status = nil
		}
		if pm.State == provider.Running && pm.Addr != "" {
			wg.Add(1)
			go func(id, addr, fp, token string) {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
				st, err := o.ProbeFunc(pctx, addr, fp, token)
				cancel()
				if err != nil {
					results <- probe{id, nil}
					return
				}
				results <- probe{id, &st}
			}(pm.ID, pm.Addr, pm.Fingerprint, o.probeToken(pm.ID))
		}
	}
	for id, m := range o.machines {
		// A machine created or changed after the listing started is not
		// missing from it.
		if !seen[id] && m.State != provider.Starting && !listedAt.Before(m.listed) {
			o.stateEventLocked(id, "gone")
			o.removeLocked(id)
			down = append(down, id)
		}
	}
	if o.seedUptime != nil {
		o.settleUptimeLocked()
	}
	cb := o.OnMachineDown
	o.mu.Unlock()
	if cb != nil {
		for _, id := range down {
			cb(id)
		}
	}
	wg.Wait()
	close(results)
	o.mu.Lock()
	for r := range results {
		m, ok := o.machines[r.id]
		if !ok {
			continue
		}
		if r.status == nil && m.Status != nil && m.State == provider.Running && time.Since(m.StatusAt) < statusGrace {
			// A single failed probe (e.g. a timeout on a loaded host) does
			// not make a running machine unschedulable: keep the last
			// status for a short while.
			continue
		}
		m.Status = r.status
		m.StatusAt = time.Now()
		o.readyLocked(m)
		o.statusHeadsLocked(r.id, m)
		o.statusVMLocked(r.id, m)
		o.statusVolumeLocked(m)
		if m.Status != nil { // nil: the probe failed (worker still starting)
			o.meter.Update(r.id, m.Provider, m.Status.Tiers)
		}
		o.sampleLocked(r.id, m)
	}
	// The data is as old as the listing, not as the end of the probes:
	// a session that ended while a slow refresh ran is not in it.
	o.refreshed = listedAt
	o.notify()
	o.mu.Unlock()
	return nil
}

// sampleLocked writes a machine_sample event from the machine's current
// status, at most every events.SampleInterval.
func (o *Orchestrator) sampleLocked(id string, m *Machine) {
	o.sessionSamplesLocked(id, m)
	st := m.Status
	if st == nil || !st.Ready || m.StatusAt.Sub(m.sampledAt) < events.SampleInterval-time.Second {
		return
	}
	m.sampledAt = m.StatusAt
	ev := o.machineEventLocked(events.MachineSample, id)
	ev.Reserved = &scheduler.Resources{CPU: st.UsedCPU, Memory: st.UsedMemory}
	used, pr := st.Measured, st.Pressure
	ev.Used, ev.Pressure, ev.Sessions = &used, &pr, len(st.Sessions)
	if d := st.Disk; d != nil {
		ev.Disk = &events.Disk{Total: d.Total, Used: d.Used, Slots: d.Slots, MachineCaches: d.MachineCaches}
	}
	if st.VM != nil {
		vm := vmOf(st.VM)
		ev.VM = &vm
	}
	o.obs.emit(ev)
	o.vmEventsLocked(id, m)
}

// vmEventsLocked logs the cache file system activity since the previous
// sample: page_fault (Pages: faults), page_in (Bytes fetched from lower
// tiers) and reclaim (Bytes dropped), each only when non zero. The worker
// counts them cumulatively; a worker restart starts a new base.
func (o *Orchestrator) vmEventsLocked(id string, m *Machine) {
	cur := m.Status.VM
	if cur == nil {
		return
	}
	prev := m.sampledVM
	c := *cur
	m.sampledVM = &c
	if prev == nil || cur.Faults < prev.Faults || cur.ReclaimBytes < prev.ReclaimBytes || cur.PageInBytes < prev.PageInBytes {
		return
	}
	emit := func(typ string, pages, bytes int64) {
		if pages > 0 || bytes > 0 {
			ev := o.machineEventLocked(typ, id)
			ev.Pages, ev.Bytes = pages, bytes
			ev.Seconds = events.SampleInterval.Seconds()
			o.obs.emit(ev)
		}
	}
	emit(events.PageFault, cur.Faults-prev.Faults, 0)
	emit(events.PageIn, 0, cur.PageInBytes-prev.PageInBytes)
	emit(events.Reclaim, 0, cur.ReclaimBytes-prev.ReclaimBytes)
}

// vmOf maps a worker's cache file system state to the scheduler's view.
func vmOf(v *proto.VMStats) scheduler.VM {
	return scheduler.VM{Capacity: v.Capacity, Resident: v.ResidentBytes, Dirty: v.DirtyBytes, WorkingSet: v.WorkingSetBytes,
		FaultRate: v.FaultRate, Thrashing: v.Thrashing}
}

// statusVMLocked logs a thrash event when a machine starts thrashing.
func (o *Orchestrator) statusVMLocked(id string, m *Machine) {
	if m.Status == nil || m.Status.VM == nil {
		return
	}
	t := m.Status.VM.Thrashing
	if t && !m.thrashing {
		vm := vmOf(m.Status.VM)
		ev := o.machineEventLocked(events.Thrash, id)
		ev.VM, ev.Bytes = &vm, vm.WorkingSet
		o.obs.emit(ev)
	}
	m.thrashing = t
}

// Sample probes the running machines (no provider listing) to refresh
// their status and write machine_sample events. It runs every
// events.SampleInterval, more often than the reconcile loop.
func (o *Orchestrator) Sample(ctx context.Context) {
	type target struct{ id, addr, fp, token string }
	var ts []target
	o.mu.Lock()
	for id, m := range o.machines {
		if m.State == provider.Running && m.Addr != "" {
			ts = append(ts, target{id, m.Addr, m.Fingerprint, o.probeToken(id)})
		}
	}
	o.mu.Unlock()
	type result struct {
		id string
		st proto.Status
	}
	results := make(chan result, len(ts))
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			if st, err := o.ProbeFunc(pctx, t.addr, t.fp, t.token); err == nil {
				results <- result{t.id, st}
			}
		}()
	}
	wg.Wait()
	close(results)
	o.mu.Lock()
	defer o.mu.Unlock()
	for r := range results {
		m, ok := o.machines[r.id]
		if !ok || m.State != provider.Running {
			continue
		}
		st := r.st
		m.Status, m.StatusAt = &st, time.Now()
		o.readyLocked(m)
		o.statusHeadsLocked(r.id, m)
		o.statusVMLocked(r.id, m)
		o.statusVolumeLocked(m)
		o.meter.Update(r.id, m.Provider, m.Status.Tiers)
		o.sampleLocked(r.id, m)
	}
}

type taggedMachine struct {
	provider.Machine
	prov string
}

// listAll lists machines of every provider. A failing provider fails the
// whole listing, so that its machines are never mistaken for gone.
func (o *Orchestrator) listAll(ctx context.Context) ([]taggedMachine, error) {
	var out []taggedMachine
	for name, p := range o.provs {
		ms, err := p.List(ctx, o.cfg.Owner)
		if err != nil {
			o.provErr(name, "list", err)
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		for _, m := range ms {
			out = append(out, taggedMachine{m, name})
		}
	}
	return out, nil
}

// provOf returns the provider of a known machine.
func (o *Orchestrator) provOf(id string) (provider.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	name := ""
	if m, ok := o.machines[id]; ok {
		name = m.Provider
	} else if c, ok := o.creating[id]; ok {
		name = c.prov
	}
	p, ok := o.provs[name]
	if !ok {
		return nil, fmt.Errorf("machine %s: unknown provider %q", id, name)
	}
	return p, nil
}

// ProbeTimeout bounds a status probe (Probe applies it when the context has
// no deadline; the orchestrator's own probes always use it). It is
// generous: on a loaded host the QUIC handshake alone may take seconds.
const ProbeTimeout = 10 * time.Second

// statusGrace is how long the last status of a running machine survives
// failed probes.
const statusGrace = 30 * time.Second

// Probe fetches a server's status.
func Probe(ctx context.Context, addr, fingerprint, token string) (proto.Status, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
	}
	conn, err := transport.Dial(ctx, addr, fingerprint)
	if err != nil {
		return proto.Status{}, err
	}
	defer conn.CloseWithError(0, "")
	_, pc, err := transport.OpenStream(ctx, conn, token, proto.KindStatus)
	if err != nil {
		return proto.Status{}, err
	}
	var st proto.Status
	err = pc.Expect(proto.TStatus, &st)
	return st, err
}

// measuredSizes returns the configured sizes with measured medium
// profiles (from the workers' tier counters) merged into every level of
// their stacks.
func (o *Orchestrator) measuredSizes() []scheduler.Size {
	sizes := o.cfg.Sizes
	copied := false
	for i, s := range sizes {
		var levels []medium.Profile
		stack := s.Stack.Levels
		if len(stack) == 0 { // no configured stack: the hot medium over the store
			hot := s.Medium
			if hot == "" {
				hot = medium.Local
			}
			stack = []medium.Profile{{Medium: hot}, {Medium: medium.Store}}
		}
		for j, l := range stack {
			mp, ok := o.meter.Profile(s.Provider, l.Medium)
			if !ok {
				continue
			}
			if levels == nil {
				levels = append([]medium.Profile(nil), stack...)
			}
			levels[j] = l.Merge(mp, "measured")
		}
		if levels == nil {
			continue
		}
		if !copied {
			sizes, copied = append([]scheduler.Size(nil), sizes...), true
		}
		sizes[i].Stack.Levels = levels
	}
	return sizes
}

// poolLocked builds the scheduler's view of the pool.
func (o *Orchestrator) poolLocked(now time.Time) scheduler.Pool {
	f := scheduler.Pool{Now: now, Sizes: o.measuredSizes(), Recent: map[string]time.Time{}, EnvArch: map[string]string{},
		Refetch: o.obs.refetch.Factors(), Arrivals: o.arrivals.Times()}
	for env, t := range o.recentEnvs {
		f.Recent[env] = t
	}
	for _, m := range o.machines {
		if a := m.Labels[provider.LabelArch]; a != "" && m.EnvHash != "" {
			f.EnvArch[m.EnvHash] = a
		}
	}
	for env, p := range o.envParams {
		if p.Arch != "" {
			f.EnvArch[env] = p.Arch
		}
	}
	for id, m := range o.machines {
		sm := scheduler.Machine{
			ID: id, Provider: m.Provider, EnvHash: m.EnvHash, Size: m.Labels[provider.LabelSize],
			Draining:  m.Draining || o.cfg.Policy.Disabled(m.Provider), // a disabled provider takes no sessions
			Capacity:  scheduler.Resources{CPU: m.CPU, Memory: m.Memory},
			CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
		}
		_, inFlight := o.creating[id]
		switch {
		case m.State == provider.Running && m.Unreachable(now):
			// It answered before and stopped: no capacity on its way
			// (queued requests must not wait for it); reconcile reboots
			// or replaces it.
			sm.State = scheduler.Gone
		case m.State == provider.Running && m.Status != nil && m.Status.Ready && m.Addr != "":
			sm.State = scheduler.Running
		case m.State == provider.Running || m.State == provider.Starting || inFlight:
			sm.State = scheduler.Starting
		case m.State == provider.Stopped:
			sm.State = scheduler.Stopped
		default:
			sm.State = scheduler.Gone
		}
		if st := m.Status; st != nil {
			sm.Reserved = scheduler.Resources{CPU: st.UsedCPU, Memory: st.UsedMemory}
			sm.Used = st.Measured
			sm.Pressure = st.Pressure
			sm.Sessions = len(st.Sessions)
			sm.WarmRepos = st.WarmRepos
			if d := st.Disk; d != nil {
				sm.DiskTotal, sm.DiskUsed, sm.SlotBytes, sm.DiskBudget = d.Total, d.Used, d.SlotBytes, d.Budget
			}
			if st.VM != nil {
				sm.VM = vmOf(st.VM)
			}
			sm.Draining = sm.Draining || st.Draining
			if st.IdleSince > 0 {
				sm.IdleSince = time.Unix(st.IdleSince, 0)
			}
		}
		for lid, p := range m.pending {
			if now.After(p.expires) {
				delete(m.pending, lid)
				continue
			}
			counted := false
			if m.Status != nil {
				for _, se := range m.Status.Sessions {
					if se.Lease == lid {
						counted = true
					}
				}
			}
			if !counted {
				sm.Reserved = sm.Reserved.Add(p.res)
				sm.IdleSince = time.Time{}
			}
		}
		f.Machines = append(f.Machines, sm)
	}
	for id, c := range o.creating {
		if _, known := o.machines[id]; known {
			continue
		}
		f.Machines = append(f.Machines, scheduler.Machine{
			ID: id, Provider: c.prov, EnvHash: c.env, Size: c.size.Name, State: scheduler.Starting,
			Capacity: c.size.Capacity, CreatedAt: c.at, ExpiresAt: c.at.Add(o.cfg.MaxLifetime),
		})
	}
	sort.Slice(f.Machines, func(i, j int) bool { return f.Machines[i].ID < f.Machines[j].ID })
	for _, t := range o.queue {
		f.Queue = append(f.Queue, t)
	}
	sort.Slice(f.Queue, func(i, j int) bool { return f.Queue[i].Since.Before(f.Queue[j].Since) })
	o.schedPoolLocked(&f)
	return f
}

// ErrQueueTimeout means no capacity became available in time.
var ErrQueueTimeout = errors.New("no machine capacity available")

// ErrUnschedulable means the policy rejected a request outright.
var ErrUnschedulable = errors.New("cannot schedule")

// Acquire reserves capacity for a session. The policy decides placement;
// while the task waits, the policy's pool plan is executed (start, create,
// stop, destroy, drain). log receives progress messages for the user.
func (o *Orchestrator) Acquire(ctx context.Context, req Request, log io.Writer) (Lease, error) {
	start := time.Now()
	ctx, span := startAcquire(ctx, req)
	lease, err := o.acquire(ctx, req, log)
	result := "assigned"
	switch {
	case err == nil:
		o.metrics.placements.Inc("assign")
	case errors.Is(err, ErrUnschedulable):
		result = "rejected"
		o.metrics.placements.Inc("reject")
	case errors.Is(err, ErrQueueTimeout):
		result = "timeout"
		o.metrics.placements.Inc("timeout")
	case ctx.Err() != nil:
		result = "canceled"
	default:
		result = "error"
	}
	o.metrics.acquire.Observe(time.Since(start).Seconds(), result)
	o.endAcquire(ctx, span, req, start, lease, result, err)
	return lease, err
}

func (o *Orchestrator) acquire(ctx context.Context, req Request, log io.Writer) (lease Lease, err error) {
	var last *scheduler.Decision
	defer func() {
		if lease.Decision == nil {
			lease.Decision = last
		}
	}()
	now := o.now()
	task := scheduler.Task{ID: newID(8), EnvHash: req.EnvHash, RepoID: req.RepoID, Key: req.Key, Request: req.Need, Limit: req.Limit, Since: now,
		Family: req.Family}
	// Fair queueing serves owners round robin: the agent's conversation,
	// else the user (empty: the repository).
	task.Owner = req.Labels[labels.AgentSession]
	if task.Owner == "" {
		task.Owner = req.Labels[labels.User]
	}
	if o.Profile != nil && req.Key != "" {
		task.Profile = o.Profile(req.Key)
	}
	o.arrivals.Observe(req.EnvHash, now)
	o.mu.Lock()
	if e := req.Estimate; e != nil && e.LearnedPeak > 0 {
		// Admission counts the learned peak, not only the estimate: the
		// sessions of a machine must fit its memory at their peaks.
		task.Request = o.cfg.Policy.PeakReservation(task.Request, e.LearnedPeak, req.Limit.Memory)
	}
	o.recentEnvs[req.EnvHash] = now
	o.noteFamilyLocked(req.Family, req.RepoID)
	o.images[req.EnvHash] = req.Image
	o.services[req.EnvHash] = req.Services
	o.envParams[req.EnvHash] = req.params()
	o.queued[task.ID] = req.params()
	o.waiting[task.ID] = QueuedTask{ID: task.ID, Since: time.Now(), RepoID: req.RepoID, Key: req.Key, Env: req.EnvHash, Need: task.Request, Labels: labels.Clean(req.Labels)}
	o.saveRecentLocked()
	stale := time.Since(o.refreshed) > 30*time.Second
	o.mu.Unlock()
	if stale {
		if err := o.Refresh(ctx); err != nil {
			return Lease{}, err
		}
	}
	defer func() {
		o.mu.Lock()
		delete(o.queue, task.ID)
		delete(o.queued, task.ID)
		delete(o.waiting, task.ID)
		o.mu.Unlock()
	}()
	deadline := now.Add(o.cfg.QueueTimeout)
	announced := map[string]bool{}
	lastWaiting := time.Now() // the first task_waiting after WaitingInterval
	refreshed := false
	for {
		o.mu.Lock()
		f := o.poolLocked(o.now())
		pl := o.policy.Place(f, task)
		if pl.Decision != nil {
			last = pl.Decision
		}
		switch pl.Kind {
		case scheduler.Assign:
			m := o.machines[pl.Machine]
			size := m.Labels[provider.LabelSize]
			lease := Lease{
				ID: task.ID, MachineID: m.ID, Addr: m.Addr, Fingerprint: m.Fingerprint, Token: o.sessionToken(m.Machine, task.ID),
				Grant: pl.Grant, MemoryOvercommit: o.cfg.Policy.MemoryOvercommit,
				Provider: m.Provider, Size: size, PricePerHour: o.machinePriceLocked(m), Reason: pl.Reason,
				BuildTTL: req.buildTTL(o.cfg.MaxLifetime), Decision: pl.Decision,
				Predicted: predicted(pl, req),
			}
			m.pending[task.ID] = pendingLease{res: pl.Grant.Reserve, expires: o.now().Add(2*time.Minute + slotTimeout)}
			exp := pl.Expected
			if exp <= 0 {
				exp = task.Profile.High(task.Profile.WarmRun())
			}
			o.leases[task.ID] = leaseInfo{start: o.now(), expected: exp, run: pl.Run, memMean: task.Profile.PeakMean, memSD: task.Profile.PeakSD}
			env := m.EnvHash
			o.mu.Unlock()
			if pl.Slot.Kind != scheduler.SlotNone {
				fmt.Fprintf(log, "offshore: preparing build slot on %s (%s)\n", m.ID, pl.Slot.Kind)
				o.prepareSlot(ctx, m.ID, pl.Slot)
			}
			lease.Store = o.storeGrant(ctx, lease.Provider, req.RepoID, env)
			return lease, nil
		case scheduler.Reject:
			o.mu.Unlock()
			return Lease{}, fmt.Errorf("%w: %s (needs %.1f CPU, %d bytes memory)", ErrUnschedulable, pl.Reason, task.Request.CPU, task.Request.Memory)
		}
		if !refreshed {
			// Placement failed on cached state: retry once with fresh state.
			o.mu.Unlock()
			refreshed = true
			if err := o.Refresh(ctx); err != nil {
				return Lease{}, err
			}
			continue
		}
		if _, queued := o.queue[task.ID]; !queued {
			o.metrics.placements.Inc("queue")
		}
		o.queue[task.ID] = task
		acts := o.planLocked()
		o.markLocked(acts)
		w := o.waitingLocked(task.EnvHash, pl)
		if qt, ok := o.waiting[task.ID]; ok {
			qt.Waiting, qt.Reason, qt.Queue = &w, pl.Reason, pl.Decision.QueueShort()
			o.waiting[task.ID] = qt
		}
		ch := o.changed
		o.mu.Unlock()
		if time.Since(lastWaiting) >= waitingInterval {
			lastWaiting = time.Now()
			o.waitingEvent(ctx, task, req, now, pl.Reason, w)
		}
		for _, a := range acts {
			msg := ""
			switch a.Kind {
			case scheduler.Start:
				msg = "starting machine " + a.Machine
			case scheduler.Create:
				msg = fmt.Sprintf("creating %s machine for environment %s", a.Size.Name, a.EnvHash[:min(12, len(a.EnvHash))])
			}
			if msg != "" && !announced[msg] {
				fmt.Fprintf(log, "offshore: %s\n", msg)
				announced[msg] = true
			}
		}
		o.execute(context.WithoutCancel(ctx), acts, 0)
		o.mu.Lock()
		blocked := o.createBlockedLocked(task.EnvHash)
		o.mu.Unlock()
		for _, b := range blocked {
			if msg := "create failed: " + b.err; !announced[msg] {
				fmt.Fprintf(log, "offshore: creating a machine failed: %s (retrying in %s)\n", b.err, b.delay)
				announced[msg] = true
			}
		}
		if len(acts) == 0 && !announced["wait"] {
			fmt.Fprintf(log, "offshore: waiting for machine capacity\n")
			announced["wait"] = true
		}
		if time.Now().After(deadline) {
			return Lease{}, ErrQueueTimeout
		}
		select {
		case <-ch:
		case <-time.After(time.Second):
			if err := o.Refresh(ctx); err != nil {
				return Lease{}, err
			}
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		}
	}
}

// markLocked records the immediate state effects of actions so that
// concurrent decisions see them (capacity on its way, machines going down).
func (o *Orchestrator) markLocked(acts []scheduler.Action) {
	for i, a := range acts {
		o.metrics.actions.Inc(actionName(a.Kind))
		o.markEventsLocked(a)
		switch a.Kind {
		case scheduler.Create:
			id := newID(6)
			acts[i].Machine = id
			o.creating[id] = creation{prov: a.Size.Provider, env: a.EnvHash, size: a.Size, at: o.now(), volume: a.Volume}
		case scheduler.Start:
			if m, ok := o.machines[a.Machine]; ok {
				m.State = provider.Starting
				m.listed = time.Now()
				m.observe(o.now())
				m.Status = nil
				o.creating[a.Machine] = creation{prov: m.Provider, env: m.EnvHash, at: o.now(), started: true}
			}
		case scheduler.Stop, scheduler.Destroy:
			if m, ok := o.machines[a.Machine]; ok {
				m.Draining = true
			}
		case scheduler.Drain:
			if m, ok := o.machines[a.Machine]; ok {
				m.Draining = true
			}
		case scheduler.Undrain:
			if m, ok := o.machines[a.Machine]; ok && !m.stopping {
				m.Draining = false
				m.undrains++
			}
		}
	}
}

// execute performs actions in the background; it waits up to wait for
// Stop and Destroy to finish (reconcile, so that tests and callers see
// their effect without blocking on slow stops).
func (o *Orchestrator) execute(ctx context.Context, acts []scheduler.Action, wait time.Duration) {
	var wg sync.WaitGroup
	for _, a := range acts {
		a := a
		switch a.Kind {
		case scheduler.Create:
			o.mu.Lock()
			image := o.images[a.EnvHash]
			o.mu.Unlock()
			go o.traced(ctx, a, func(ctx context.Context) { o.createMachine(ctx, a.Machine, a.EnvHash, image, a.Size) })
		case scheduler.Start:
			go o.traced(ctx, a, func(ctx context.Context) { o.startMachine(ctx, a.Machine) })
		case scheduler.Stop, scheduler.Destroy:
			o.mu.Lock()
			busy := o.downing[a.Machine]
			o.downing[a.Machine] = true
			o.mu.Unlock()
			if busy { // a stop or destroy of it is still running
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					o.mu.Lock()
					delete(o.downing, a.Machine)
					o.mu.Unlock()
				}()
				o.traced(ctx, a, func(ctx context.Context) {
					if a.Kind == scheduler.Stop {
						o.stopMachine(ctx, a.Machine, a.Reason)
					} else {
						o.destroyMachine(ctx, a.Machine, a.Reason)
					}
				})
			}()
		}
	}
	if wait <= 0 {
		return
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		// Stops save build slots first and may take minutes: they finish
		// in the background (downing keeps them from being repeated).
	}
}

// commitDown decides, after the slots were saved (which takes minutes),
// whether a stop or destroy still proceeds: a machine that was undrained
// meanwhile or got a lease (a session placed on it) keeps running, since
// stopping it would kill those sessions. Otherwise it is marked stopping
// and is not undrained any more.
func (o *Orchestrator) commitDown(id, what, reason string, undrains int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	m, ok := o.machines[id]
	if !ok {
		return true
	}
	if m.undrains != undrains || len(m.pending) > 0 {
		o.log.Warn(what+" of machine cancelled: it took work while its slots were saved", "id", id, "reason", reason,
			"undrained", m.undrains != undrains, "leases", len(m.pending))
		m.Draining = false
		o.refreshed = time.Time{}
		o.notify()
		return false
	}
	m.stopping = true
	return true
}

// undrainsOf returns the machine's undrain count (see commitDown).
func (o *Orchestrator) undrainsOf(id string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if m, ok := o.machines[id]; ok {
		return m.undrains
	}
	return 0
}

func (o *Orchestrator) stopMachine(ctx context.Context, id, reason string) {
	u := o.undrainsOf(id)
	o.saveBeforeStop(ctx, id, "stop: "+reason)
	if !o.commitDown(id, "stop", reason, u) {
		return
	}
	defer func() {
		o.mu.Lock()
		if m, ok := o.machines[id]; ok {
			m.stopping = false
		}
		o.mu.Unlock()
	}()
	o.log.Info("stopping machine", "id", id, "reason", reason)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("stop", "id", id, "err", err)
		return
	}
	o.setState(id, provider.Stopped)
	if err := o.provErr(o.provName(p), "stop", p.Stop(ctx, id)); err != nil {
		o.log.Error("stop", "id", id, "err", err)
	}
	o.mu.Lock()
	if m, ok := o.machines[id]; ok {
		m.Draining = false
	}
	o.mu.Unlock()
}

func (o *Orchestrator) destroyMachine(ctx context.Context, id, reason string) {
	if reason != "failed" {
		u := o.undrainsOf(id)
		o.saveBeforeStop(ctx, id, "destroy: "+reason)
		if !o.commitDown(id, "destroy", reason, u) {
			return
		}
	}
	o.log.Info("destroying machine", "id", id, "reason", reason)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("destroy", "id", id, "err", err)
		return
	}
	o.setState(id, provider.Failed)
	if err := o.provErr(o.provName(p), "destroy", p.Destroy(ctx, id)); err != nil {
		o.log.Error("destroy", "id", id, "err", err)
		return
	}
	o.mu.Lock()
	o.removeLocked(id)
	o.mu.Unlock()
	go o.releaseVolume(context.WithoutCancel(ctx), id)
}

func (o *Orchestrator) setState(id string, st provider.State) {
	o.mu.Lock()
	if m, ok := o.machines[id]; ok {
		m.State = st
		m.listed = time.Now()
		m.observe(o.now())
		m.Status = nil
		m.Addr = ""
	}
	cb := o.OnMachineDown
	o.mu.Unlock()
	if cb != nil {
		cb(id)
	}
}

// Unreachable reports that a leased machine could not be reached; its
// cached state is dropped so the next placement re-probes it.
func (o *Orchestrator) Unreachable(leaseID, machineID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if m, ok := o.machines[machineID]; ok {
		delete(m.pending, leaseID)
		m.Status = nil
	}
	o.refreshed = time.Time{}
	o.notify()
}

// Release drops a pending lease (the session has ended or never started).
func (o *Orchestrator) Release(leaseID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.leases, leaseID)
	for _, m := range o.machines {
		delete(m.pending, leaseID)
	}
	o.notify()
}

func (o *Orchestrator) createMachine(ctx context.Context, id, envHash, image string, size scheduler.Size) {
	defer func() {
		o.mu.Lock()
		delete(o.creating, id)
		o.notify()
		o.mu.Unlock()
	}()
	start := time.Now()
	err := o.CreateMachine(ctx, id, envHash, image, size)
	o.mu.Lock()
	if err == nil {
		o.providerReadyLocked(size.Provider, time.Since(start), false)
		delete(o.createFail, scheduler.CreateKey(envHash, size.Provider))
		o.mu.Unlock()
		return
	}
	var delay time.Duration
	if !o.providerFailedLocked(size.Provider, err) {
		delay = o.createFailedLocked(envHash, size.Provider, err)
	}
	// The machine_create event opened a machine record: close it when no
	// machine exists, so failed creates do not accumulate as running
	// machines (one event per failure).
	if _, exists := o.machines[id]; !exists {
		ev := o.machineEventLocked(events.MachineDestroy, id)
		ev.Env, ev.Provider, ev.Size = envHash, size.Provider, size.Name
		ev.Reason, ev.Error, ev.ErrorClass = "create_failed", err.Error(), string(provider.Classify(err))
		o.obs.emit(ev)
	}
	o.mu.Unlock()
	o.log.Error("create machine", "id", id, "provider", size.Provider, "err", err, "retry_in", delay)
}

// paramsLocked returns the repository parameters for a new machine of an
// environment: those of the longest waiting request for it, else of its last
// request, else the defaults.
func (o *Orchestrator) paramsLocked(envHash string) repoParams {
	var best scheduler.Task
	found := false
	for id, t := range o.queue {
		if _, ok := o.queued[id]; ok && t.EnvHash == envHash && (!found || t.Since.Before(best.Since)) {
			best, found = t, true
		}
	}
	if found {
		return o.queued[best.ID]
	}
	if p, ok := o.envParams[envHash]; ok {
		return p
	}
	return Request{}.params()
}

// CreateMachine provisions a new machine of size and waits until it is ready.
func (o *Orchestrator) CreateMachine(ctx context.Context, id, envHash, image string, size scheduler.Size) error {
	o.mu.Lock()
	cfg := o.cfg
	services := o.services[envHash]
	params := o.paramsLocked(envHash)
	o.mu.Unlock()
	// Whole seconds: the expiry travels as Unix seconds and is an input of
	// the derived certificate.
	expires := time.Unix(o.now().Add(cfg.MaxLifetime).Unix(), 0)
	seed := o.master.MachineSeed(id)
	ident, err := auth.MachineIdentity(seed, id, expires)
	if err != nil {
		return err
	}
	provision, _ := json.Marshal(cfg.Provision)
	spec := provider.MachineSpec{
		ID: id, Owner: cfg.Owner, Image: image, EnvHash: envHash, Size: size.Name, PricePerHour: size.PricePerHour,
		Arch: size.ArchOf(), InstanceTypes: size.InstanceTypes,
		CPU: size.Capacity.CPU, Memory: size.Capacity.Memory, Swap: cfg.Swap,
		ExpiresAt: expires, Finger: ident.Fingerprint, DepsTTL: params.DepsTTL,
		IdleStop: selfStop(cfg.Policy.IdleStop), Services: services,
		Env: map[string]string{
			"OFFSHORE_MACHINE_ID": id,
			"OFFSHORE_ENV_HASH":   envHash,
			"OFFSHORE_EXPIRES_AT": strconv.FormatInt(expires.Unix(), 10),
			"OFFSHORE_CPU":        strconv.FormatFloat(size.Capacity.CPU, 'f', -1, 64),
			"OFFSHORE_MEMORY":     strconv.FormatInt(size.Capacity.Memory, 10),
			"OFFSHORE_PROVISION":  string(provision),
			auth.EnvSignerPub:     o.master.SignerPublic(),
			auth.EnvFingerprint:   ident.Fingerprint,
		},
		SecretEnv: map[string]string{
			auth.EnvTLSSeed: auth.EncodeKey(seed),
		},
	}
	// The machine-wide build TTL is only an upper bound: every session
	// sends its repository's TTL, which the server records per build slot.
	if cfg.MaxLifetime > 0 {
		spec.Env["OFFSHORE_BUILD_TTL"] = cfg.MaxLifetime.String()
	}
	if size.DiskBudget != "" {
		spec.Env["OFFSHORE_DISK_BUDGET"] = size.DiskBudget
	}
	if cfg.DirtyRatio > 0 {
		spec.Env["OFFSHORE_WRITEBACK_DIRTY_RATIO"] = strconv.FormatFloat(cfg.DirtyRatio, 'f', -1, 64)
	}
	if cfg.DirtyExpire > 0 {
		spec.Env["OFFSHORE_WRITEBACK_DIRTY_EXPIRE"] = cfg.DirtyExpire.String()
	}
	if cfg.SourceCache != "" {
		spec.Env["OFFSHORE_SOURCE_CACHE"] = cfg.SourceCache
	}
	for k, v := range logs.Forward() { // machines log like the pool
		spec.Env[k] = v
	}
	for k, v := range tracing.Forward() { // and trace to the same backend
		spec.Env[k] = v
	}
	for k, v := range tracing.ForwardSecret() {
		spec.SecretEnv[k] = v
	}
	prov, ok := o.provs[size.Provider]
	if !ok {
		return fmt.Errorf("size %s: unknown provider %q", size.Name, size.Provider)
	}
	if o.PrepareImage != nil {
		ref, err := o.PrepareImage(ctx, size.Provider, prov, envHash, size.Name)
		if err != nil {
			return err
		}
		if ref != "" {
			spec.Image = ref
		}
	}
	var vol *VolumeState
	if size.Medium == medium.Volume {
		o.mu.Lock()
		want, repo := o.creating[id].volume, o.waitingRepoLocked(envHash)
		o.mu.Unlock()
		if vol, err = o.acquireVolume(ctx, id, size, want, repo); err != nil {
			return err
		}
		spec.Volume, spec.CacheTTL = vol.ID, vol.Policy.TTL
	}
	o.log.Info("creating machine", "id", id, "provider", size.Provider, "size", size.Name, "env", envHash[:min(12, len(envHash))], "image", spec.Image, "volume", spec.Volume)
	m, err := prov.Create(ctx, spec)
	if err != nil {
		if vol != nil {
			go o.releaseVolume(context.WithoutCancel(ctx), id)
		}
		return o.provErr(size.Provider, "create", err)
	}
	if vol != nil {
		o.attachedVolume(vol, m.Zone)
	}
	m.Fingerprint = ident.Fingerprint
	o.mu.Lock()
	o.providedLocked(id)
	nm := &Machine{Machine: m, Provider: size.Provider, pending: map[string]pendingLease{}, listed: time.Now(), price: size.PricePerHour}
	nm.observe(o.now())
	o.machines[id] = nm
	o.mu.Unlock()
	return o.waitReady(ctx, id)
}

func (o *Orchestrator) startMachine(ctx context.Context, id string) {
	defer func() {
		o.mu.Lock()
		delete(o.creating, id)
		o.notify()
		o.mu.Unlock()
	}()
	o.log.Info("starting machine", "id", id)
	p, err := o.provOf(id)
	if err != nil {
		o.log.Error("start machine", "id", id, "err", err)
		return
	}
	startedAt := time.Now()
	m, err := p.Start(ctx, id)
	if err != nil {
		o.provErr(o.provName(p), "start", err)
		o.log.Error("start machine", "id", id, "err", err)
		o.mu.Lock()
		// No start loop: back off before trying this machine again
		// (e.g. still stopping, or no capacity for it).
		o.startBlocked[id] = o.now().Add(startBackoff)
		o.providerFailedLocked(o.provName(p), err)
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	o.providedLocked(id)
	if mm, ok := o.machines[id]; ok {
		mm.update(m, startedAt)
		mm.observe(o.now())
		mm.Status = nil
		mm.Draining = false
	}
	o.mu.Unlock()
	if err := o.waitReady(ctx, id); err != nil {
		o.log.Error("machine did not become ready", "id", id, "err", err)
		return
	}
	o.mu.Lock()
	delete(o.startBlocked, id)
	o.providerReadyLocked(o.provName(p), time.Since(startedAt), true)
	o.mu.Unlock()
}

// pollIntervals returns the readiness poll interval of a provider and its
// backoff ceiling.
func pollIntervals(c provider.Caps) (first, max time.Duration) {
	first = c.PollInterval
	if first <= 0 {
		first = 500 * time.Millisecond
	}
	max = c.PollMax
	if max < first {
		max = first
	}
	return first, max
}

// nextPoll backs the poll interval off by half up to max.
func nextPoll(cur, max time.Duration) time.Duration {
	return min(cur+cur/2, max)
}

// Readiness polling of a booting machine: once the provider reports its
// address, the server is probed every bootProbeEvery (each probe bounded
// by bootProbeTimeout: a server not yet listening does not answer) and the
// provider listed only every bootListEvery (to notice a machine that
// stopped); until then the provider is listed at its poll interval.
const (
	bootProbeEvery   = time.Second
	bootProbeTimeout = 3 * time.Second
	bootListEvery    = 10 * time.Second
)

// waitReady polls the machine until its server reports ready.
func (o *Orchestrator) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(10 * time.Minute)
	p, perr := o.provOf(id)
	if perr != nil {
		return perr
	}
	interval, maxInterval := pollIntervals(p.Caps())
	var addr, finger string
	var listed time.Time
	for time.Now().Before(deadline) {
		if addr == "" || time.Since(listed) >= bootListEvery {
			listedAt := time.Now()
			list, err := p.List(ctx, o.cfg.Owner)
			o.provErr(o.provName(p), "list", err)
			if err == nil {
				listed = listedAt
				for _, pm := range list {
					if pm.ID != id {
						continue
					}
					if pm.State == provider.Stopped || pm.State == provider.Failed {
						return fmt.Errorf("machine %s stopped during startup", id)
					}
					pm.Fingerprint = o.fingerprint(pm)
					o.mu.Lock()
					fresh := false
					if m := o.machines[id]; m != nil {
						fresh = m.update(pm, listedAt)
						m.observe(o.now())
					}
					o.mu.Unlock()
					if fresh {
						addr, finger = pm.Addr, pm.Fingerprint
					}
				}
			}
		}
		if addr != "" {
			pctx, cancel := context.WithTimeout(ctx, bootProbeTimeout)
			st, err := o.ProbeFunc(pctx, addr, finger, o.probeToken(id))
			cancel()
			if err == nil {
				o.mu.Lock()
				if m := o.machines[id]; m != nil {
					m.Status, m.StatusAt = &st, time.Now()
					o.readyLocked(m)
				}
				o.notify()
				o.mu.Unlock()
				if st.Ready {
					o.readyEvent(ctx, id, st.Boot)
					o.log.Info("machine ready", "id", id, "addr", addr)
					return nil
				}
			}
		}
		wait := bootProbeEvery
		if addr == "" {
			wait = interval
			interval = nextPoll(interval, maxInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("machine %s not ready after 10m", id)
}

// Snapshot returns the current machines.
func (o *Orchestrator) Snapshot() []Machine {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Machine
	now := o.now()
	for _, m := range o.machines {
		m.observe(now)
		c := *m
		c.pending = nil
		c.Uptime = append([]Span(nil), m.Uptime...)
		c.PricePerHour = o.machinePriceLocked(m)
		out = append(out, c)
	}
	kept := o.removed[:0]
	for _, m := range o.removed {
		if now.Sub(m.Removed) > historyKeep {
			continue
		}
		kept = append(kept, m)
		c := *m
		c.Uptime = append([]Span(nil), m.Uptime...)
		c.PricePerHour = o.machinePriceLocked(m)
		out = append(out, c)
	}
	o.removed = kept
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// machinePriceLocked returns a machine's hourly price: recorded on the
// machine (LabelPrice, set at creation), else seen in its events, else
// the configured price of its size (0: unknown). A size removed from the
// configuration keeps the price its machines were created with.
func (o *Orchestrator) machinePriceLocked(m *Machine) float64 {
	if v, err := strconv.ParseFloat(m.Labels[provider.LabelPrice], 64); err == nil && v > 0 {
		return v
	}
	if m.price > 0 {
		return m.price
	}
	return o.priceLocked(m.Provider, m.Labels[provider.LabelSize])
}

// priceLocked returns the hourly price of a pool size (0: unknown).
func (o *Orchestrator) priceLocked(prov, size string) float64 {
	for _, sz := range o.cfg.Sizes {
		if sz.Name == size && (sz.Provider == prov || sz.Provider == "") {
			return sz.PricePerHour
		}
	}
	return 0
}

// removeLocked forgets a machine that no longer exists, keeping its uptime
// for cost accounting.
func (o *Orchestrator) removeLocked(id string) {
	m, ok := o.machines[id]
	if !ok {
		return
	}
	delete(o.machines, id)
	o.meter.Forget(id)
	now := o.now()
	m.State = provider.Failed
	m.observe(now)
	m.Removed = now
	m.Status, m.pending, m.Addr = nil, nil, ""
	if len(m.Uptime) > 0 {
		o.removed = append(o.removed, m)
		o.trimRemovedLocked()
	}
}

// Reconcile enforces hard invariants (expired and failed machines are
// destroyed, expiring busy machines drain, orphaned volumes are deleted)
// and executes the policy's pool plan.
func (o *Orchestrator) Reconcile(ctx context.Context) error {
	o.localDisksOnce.Do(func() { o.resolveLocalDisks(ctx) })
	start := time.Now()
	ctx, span := tracing.Start(ctx, "orchestrator.reconcile")
	err := o.reconcile(ctx)
	tracing.End(span, err)
	result := "ok"
	if err != nil {
		result = "error"
	}
	o.metrics.reconcile.Observe(time.Since(start).Seconds(), result)
	return err
}

func (o *Orchestrator) reconcile(ctx context.Context) error {
	if err := o.Refresh(ctx); err != nil {
		return err
	}
	go o.refreshQuotas(context.WithoutCancel(ctx)) // bounded, at most every quotaRefresh
	now := o.now()
	o.mu.Lock()
	var invariant []scheduler.Action
	// A machine counts as unhealthy only while another machine of its
	// provider is ready: when none answers, the network of the control
	// plane is the likelier cause.
	reachable := map[string]bool{}
	for _, m := range o.machines {
		if m.State == provider.Running && m.Status != nil && m.Status.Ready {
			reachable[m.Provider] = true
		}
	}
	queued := map[string]bool{}
	for _, q := range o.waiting {
		queued[q.Env] = true
	}
	var reboot []string
	for _, m := range o.machines {
		expired := !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
		busy := m.Status != nil && len(m.Status.Sessions) > 0
		_, inFlight := o.creating[m.ID]
		switch {
		case m.State == provider.Failed:
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: "failed"})
		case o.cfg.Policy.Disabled(m.Provider) && !inFlight && m.State != provider.Starting && !busy && len(m.pending) == 0:
			// enabled = false: the provider is disabled; its machines
			// finish their sessions and are destroyed.
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: "provider disabled"})
		case expired && m.State == provider.Running && busy:
			m.Draining = true
		case expired && m.State != provider.Starting:
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: "expired"})
		case !inFlight && o.replaceable(m) && m.Unreachable(now) && queued[m.EnvHash]:
			// Its worker answered before and stopped (incident 2026-11-12:
			// the host ran out of memory) while requests wait for its
			// environment: reboot it, and replace it when it still does
			// not answer RebootGrace later. Also the only machine of its
			// provider (the queued requests need it, whatever the cause).
			_, canReboot := o.provs[m.Provider].(provider.Rebooter)
			rebooted := m.rebootedAt.After(m.LastReady)
			switch {
			case canReboot && !rebooted:
				m.rebootedAt = now
				reboot = append(reboot, m.ID)
			case !rebooted || now.Sub(m.rebootedAt) > RebootGrace:
				reason := "unreachable for " + m.UnreachableFor(now).Round(time.Second).String()
				if rebooted {
					reason += ", " + now.Sub(m.rebootedAt).Round(time.Second).String() + " after a reboot"
				}
				o.log.Warn("replacing unreachable machine", "id", m.ID, "reason", reason)
				invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: reason})
			}
		case !inFlight && len(m.pending) == 0 && o.replaceable(m) && m.NotReadyFor(now) > UnhealthyAfter && reachable[m.Provider]:
			// Up at the provider, but its worker has not reported ready
			// for long (a boot that never finished, a dead or wedged
			// worker): no capacity, only cost. Replace it.
			reason := "unhealthy: worker not ready for " + m.NotReadyFor(now).Round(time.Second).String()
			o.log.Warn("replacing unhealthy machine", "id", m.ID, "reason", reason)
			invariant = append(invariant, scheduler.Action{Kind: scheduler.Destroy, Machine: m.ID, Reason: reason})
		}
	}
	for env, used := range o.recentEnvs {
		if now.Sub(used) > 24*time.Hour {
			delete(o.recentEnvs, env)
		}
	}
	o.markLocked(invariant)
	o.mu.Unlock()
	for _, id := range reboot {
		o.rebootMachine(context.WithoutCancel(ctx), id)
	}
	o.execute(context.WithoutCancel(ctx), invariant, reconcileWait)

	o.mu.Lock()
	acts := o.planLocked()
	o.markLocked(acts)
	o.mu.Unlock()
	o.execute(context.WithoutCancel(ctx), acts, reconcileWait)
	return o.reapVolumes(ctx)
}

// rebootMachine reboots an unreachable machine at its provider.
func (o *Orchestrator) rebootMachine(ctx context.Context, id string) {
	o.mu.Lock()
	m := o.machines[id]
	var p provider.Provider
	if m != nil {
		p = o.provs[m.Provider]
	}
	o.mu.Unlock()
	r, ok := p.(provider.Rebooter)
	if !ok {
		return
	}
	o.log.Warn("rebooting unreachable machine", "id", id)
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := r.Reboot(rctx, id); err != nil {
		// Replaced RebootGrace later (rebootedAt is set).
		o.log.Error("reboot failed", "id", id, "err", err)
	}
}

// reapVolumes deletes expired and orphaned volumes on every provider.
func (o *Orchestrator) reapVolumes(ctx context.Context) error {
	for name, p := range o.provs {
		if err := o.reapProvider(ctx, p); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
	}
	return nil
}

// reapProvider deletes a provider's detached volumes that are expired or
// orphaned: labelled with a machine (a per-machine cache volume, or one
// attached to a machine that has gone) that no longer exists, and without
// an expiry that keeps them for reuse.
func (o *Orchestrator) reapProvider(ctx context.Context, p provider.Provider) error {
	pv := provider.Split(p).Volumes
	if pv == nil {
		return nil
	}
	vols, err := pv.ListVolumes(ctx, o.cfg.Owner)
	if err != nil {
		return o.provErr(o.provName(p), "volumes", err)
	}
	list, err := p.List(ctx, o.cfg.Owner)
	if err != nil {
		return o.provErr(o.provName(p), "list", err)
	}
	alive := map[string]bool{}
	for _, m := range list {
		alive[m.ID] = true
	}
	// A machine being created has its cache volume before the provider
	// lists the machine: that volume is not orphaned. (o.creating is read
	// after the listing, so a creation that finished in between is listed.)
	o.mu.Lock()
	for id := range o.creating {
		alive[id] = true
	}
	o.mu.Unlock()
	o.syncVolumes(ctx, o.provName(p), pv, vols)
	// Managed volumes whose machine disappeared without a destroy (it
	// terminated itself at expiry) are released like destroyed ones.
	o.mu.Lock()
	var gone []string
	for _, v := range o.volumes {
		if v.Machine != "" && v.Provider == o.provName(p) && !alive[v.Machine] && o.machines[v.Machine] == nil {
			gone = append(gone, v.Machine)
		}
	}
	o.mu.Unlock()
	for _, id := range gone {
		go o.releaseVolume(context.WithoutCancel(ctx), id)
	}
	now := o.now()
	for _, v := range vols {
		o.mu.Lock()
		managed := o.managedLocked(v.ID)
		o.mu.Unlock()
		if v.State != provider.VolumeAvailable || managed {
			continue // attached, changing state, or managed (syncVolumes)
		}
		owner := v.Labels[provider.LabelMachine]
		expired := v.Expired(now)
		orphan := v.Kind == provider.VolumeCache && owner != "" && !alive[owner] && v.ExpiresAt.IsZero()
		if expired || orphan {
			if err := o.provErr(o.provName(p), "delete_volume", pv.DeleteVolume(ctx, v.ID)); err == nil {
				o.log.Info("deleted volume", "provider", p.Name(), "volume", v.ID, "name", v.Name, "expired", expired, "orphan", orphan)
			}
		}
	}
	return nil
}

// DestroyAll removes every machine and volume of the owner on every provider.
func (o *Orchestrator) DestroyAll(ctx context.Context) error {
	for name, p := range o.provs {
		list, err := p.List(ctx, o.cfg.Owner)
		if err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		for _, m := range list {
			if err := p.Destroy(ctx, m.ID); err != nil {
				return fmt.Errorf("provider %s: %w", name, err)
			}
		}
		pv := provider.Split(p).Volumes
		if pv == nil {
			continue
		}
		vols, err := pv.ListVolumes(ctx, o.cfg.Owner)
		if err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		for _, v := range vols {
			if v.Machine != "" && v.State == provider.VolumeAttached {
				pv.DetachVolume(ctx, v.ID, time.Time{})
			}
			pv.DeleteVolume(ctx, v.ID)
		}
	}
	o.mu.Lock()
	for id := range o.machines {
		o.removeLocked(id)
	}
	o.machines = map[string]*Machine{}
	o.volumes = map[string]*VolumeState{}
	o.saveVolumesLocked()
	o.recentEnvs = map[string]time.Time{}
	o.saveRecentLocked()
	o.mu.Unlock()
	return nil
}

// Queue returns the requests currently waiting for capacity, oldest first.
// A request counts from its arrival, including placement on cached state.
func (o *Orchestrator) Queue() []QueuedTask {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]QueuedTask, 0, len(o.waiting))
	for id, t := range o.waiting {
		if _, queued := o.queue[id]; queued {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// SelfStopFallback is when a machine stops itself while idle and not
// observed by an orchestrator, when idle_stop is unset (the lifecycle
// policy stops idle machines while the orchestrator runs).
const SelfStopFallback = 20 * time.Minute

// selfStop is the idle time after which an unobserved machine stops
// itself: idle_stop, else SelfStopFallback.
func selfStop(idleStop time.Duration) time.Duration {
	if idleStop > 0 {
		return idleStop
	}
	return SelfStopFallback
}
