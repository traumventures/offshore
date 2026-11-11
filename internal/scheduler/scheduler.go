// Package scheduler contains the scheduling decisions of offshore as
// pure functions over a pool snapshot: where a task runs (Place), how the
// pool changes (Plan) and how much a task needs (Estimator).
//
// The orchestrator gathers state, calls a Policy and executes its
// decisions; it contains no placement or scaling logic itself. Policies can
// therefore be swapped, tuned and tested (or simulated) without machines.
package scheduler

import (
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

// VolumePolicy sizes the cache volumes of a volume size
// ([providers.NAME.volume]).
type VolumePolicy struct {
	Min          int64         `json:"min,omitempty"`  // size of a new volume
	Max          int64         `json:"max,omitempty"`  // largest size (0: provider limit)
	Step         int64         `json:"step,omitempty"` // least growth of a resize
	KeepDetached bool          `json:"keep_detached,omitempty"`
	TTL          time.Duration `json:"ttl,omitempty"`
	Type         string        `json:"type,omitempty"`
	IOPS         int           `json:"iops,omitempty"`
	Throughput   int           `json:"throughput,omitempty"`
	VPUs         int           `json:"vpus,omitempty"`
}

// Volume is a detached cache volume the pool keeps for reuse.
type Volume struct {
	ID        string
	Provider  string
	Zone      string
	Bytes     int64
	SlotBytes map[string]int64 // warm build slots on it (repository -> bytes)
	ExpiresAt time.Time
}

// Warm reports whether v holds a build slot of repo.
func (v Volume) Warm(repo string) bool { return v.SlotBytes[repo] > 0 }

// Resources is an amount of CPU (cores), memory (bytes) and disk (bytes).
// Disk is not reserved per session: build slots persist on a machine's
// disk, so it is planned by the cost model (slot sizes, growth), not
// packed. Fits checks disk only when o asks for some.
type Resources struct {
	CPU    float64 `json:"cpu"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk,omitempty"`
}

// Add returns r + o.
func (r Resources) Add(o Resources) Resources {
	return Resources{CPU: r.CPU + o.CPU, Memory: r.Memory + o.Memory, Disk: r.Disk + o.Disk}
}

// Sub returns r - o.
func (r Resources) Sub(o Resources) Resources {
	return Resources{CPU: r.CPU - o.CPU, Memory: r.Memory - o.Memory, Disk: r.Disk - o.Disk}
}

// Fits reports whether o fits into r.
func (r Resources) Fits(o Resources) bool {
	return o.CPU <= r.CPU && o.Memory <= r.Memory && (o.Disk <= 0 || o.Disk <= r.Disk)
}

// Scale multiplies CPU and memory by separate factors (disk unchanged).
func (r Resources) Scale(cpu, mem float64) Resources {
	return Resources{CPU: r.CPU * cpu, Memory: int64(float64(r.Memory) * mem), Disk: r.Disk}
}

// Size is a machine type a backend can create.
type Size struct {
	Provider     string    `json:"provider"` // provider instance that creates it
	Name         string    `json:"name"`
	Capacity     Resources `json:"capacity"`       // Disk: the machine disk (0 = unknown)
	PricePerHour float64   `json:"price_per_hour"` // 0 when unknown
	// Arch is the machine architecture ("amd64", "arm64"; empty: amd64).
	// Machines are only created for environments of their architecture.
	Arch string `json:"arch,omitempty"`
	// InstanceTypes are passed to the provider: machine types providing
	// this size, tried in order on capacity errors.
	InstanceTypes []string `json:"instance_types,omitempty"`
	// DiskBudget is how much of the cache disk machines of this size may
	// fill before pruning ("90%" or "150G"; empty = the worker's default),
	// passed to the worker.
	DiskBudget string `json:"disk_budget,omitempty"`
	// Medium is where machines of the size keep their caches: "local"
	// (default) or "volume" (Volume). Stack is the composite capability
	// profile of the size's media (hot medium over the store).
	Medium string       `json:"medium,omitempty"`
	Stack  medium.Stack `json:"stack,omitzero"`
	Volume VolumePolicy `json:"volume,omitzero"`
}

// ArchOf returns a size's architecture (amd64 when unset).
func (s Size) ArchOf() string {
	if s.Arch == "" {
		return "amd64"
	}
	return s.Arch
}

// Task is a session that needs capacity.
type Task struct {
	ID      string
	EnvHash string
	RepoID  string
	Key     string    // repository + tool + subcommand, the unit of learning
	Request Resources // estimated need, used for packing
	Limit   Resources // hard ceiling for the session; 0 = machine capacity
	Since   time.Time // when the task started waiting
	// Profile is what the estimator knows about runs of Key (durations,
	// slot growth); zero = unknown.
	Profile Profile
}

// State of a machine as seen by the scheduler.
type State int

const (
	Running  State = iota // ready to accept sessions
	Starting              // being created or started; capacity on its way
	Stopped               // compute released, disk (warm caches) kept
	Gone                  // failed or being destroyed
)

// Pressure is Linux PSI "some avg10" as a fraction (0..1).
type Pressure struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

// Machine is the scheduler's view of one machine.
type Machine struct {
	ID        string
	Provider  string
	EnvHash   string
	Size      string
	State     State
	Draining  bool      // no new sessions; stop when empty
	Capacity  Resources // physical capacity
	Reserved  Resources // sum of reservations of sessions and pending leases
	Used      Resources // measured usage (best effort)
	Pressure  Pressure
	Sessions  int
	WarmRepos []string // repositories with an unexpired build cache
	// DiskTotal and DiskUsed are the machine disk (0 = unknown); SlotBytes
	// the size of each repository's build slot (repo id -> bytes).
	DiskTotal int64
	DiskUsed  int64
	// DiskBudget is the bytes the worker lets the disk fill before pruning
	// (0 = unknown: diskFull of DiskTotal).
	DiskBudget int64
	SlotBytes  map[string]int64
	IdleSince  time.Time // zero while sessions run
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// ReadyAt is when a starting machine is expected to accept sessions
	// (zero: unknown, the provider's boot or start latency applies).
	ReadyAt time.Time
	// Releases are the expected ends of the running sessions.
	Releases []Release
	// StartBlocked: a start failed recently; do not start the machine
	// before this time (backoff).
	StartBlocked time.Time
	// CanResize: the machine's disk can be grown (provider support).
	CanResize bool
	// Medium, Zone and Volume (id) of the machine's cache; Stack the
	// composite capability profile; ResizableAt is when its disk may be
	// resized next (EBS: 6 h after the last resize).
	Medium      string
	Zone        string
	Volume      string
	Stack       medium.Stack
	ResizableAt time.Time
}

// Release is the expected end of a session and the reservation it frees.
type Release struct {
	At    time.Time
	Res   Resources
	Start time.Time // session start (zero: unknown)
}

// Expected returns when r is expected at now: sessions running longer than
// expected are assumed to run as long again as they already did (at least
// 30s more).
func (r Release) Expected(now time.Time) time.Time {
	if !r.At.Before(now) {
		return r.At
	}
	ran := now.Sub(r.At)
	if !r.Start.IsZero() {
		ran = now.Sub(r.Start)
	}
	return now.Add(max(ran, 30*time.Second))
}

// ProviderState is what the scheduler knows about a provider instance.
type ProviderState struct {
	// Boot and Start are the latencies until a created or started machine
	// accepts sessions (learned; 0 = defaults).
	Boot  time.Duration
	Start time.Duration
	// Down: the provider reported no capacity or quota; no machines are
	// created there before this time.
	Down time.Time
	// Store is the name of the provider's object store (snapshots, global
	// caches); empty = the provider name.
	Store string
}

// Demand is the recent demand of an environment (see Forecast).
type Demand struct {
	// Peak is the largest reservation (running plus queued) of the
	// environment's sessions in the forecast window.
	Peak Resources
	// LastScaleUp is the last create, start or undrain for it.
	LastScaleUp time.Time
}

// Warm reports whether the machine holds a build cache for repo.
func (m Machine) Warm(repo string) bool {
	for _, r := range m.WarmRepos {
		if r == repo {
			return true
		}
	}
	return false
}

// Pool is a snapshot of everything the scheduler may decide on.
type Pool struct {
	Now      time.Time
	Machines []Machine
	Queue    []Task               // tasks waiting for capacity
	Sizes    []Size               // machine types that can be created
	Recent   map[string]time.Time // environment hash -> last use
	// EnvArch is the machine architecture of each known environment
	// ("amd64", "arm64"); environments without an entry are amd64.
	EnvArch map[string]string
	// Providers holds latencies, health and stores per provider instance.
	Providers map[string]ProviderState
	// Demand is the recent demand per environment (nil: unknown, then
	// consolidation only looks at the present).
	Demand map[string]Demand
	// Snapshots are the saved build slots per repository.
	Snapshots map[string][]Snapshot
	// Volumes are detached cache volumes kept for reuse (keep_detached):
	// a new machine of a volume size in their zone gets their warm slots
	// for the attach time.
	Volumes []Volume
}

// SizesFor returns the sizes that can be created for env now: those of
// its architecture whose provider is not in a capacity cooldown.
func (f Pool) SizesFor(env string) []Size {
	var out []Size
	for _, s := range f.ArchSizes(env) {
		if !providerDown(f, s.Provider) {
			out = append(out, s)
		}
	}
	return out
}

// ArchSizes returns the sizes whose architecture matches env's.
func (f Pool) ArchSizes(env string) []Size {
	arch := f.EnvArch[env]
	if arch == "" {
		arch = "amd64"
	}
	var out []Size
	for _, s := range f.Sizes {
		if s.ArchOf() == arch {
			out = append(out, s)
		}
	}
	return out
}

// PlacementKind is the outcome of Place.
type PlacementKind int

const (
	Wait   PlacementKind = iota // no capacity yet; Plan decides how to get some
	Assign                      // run on Placement.Machine
	Reject                      // the task can never be placed (too large)
)

// Grant is what a session is given on its machine.
type Grant struct {
	Reserve    Resources `json:"reserve"`     // counted against machine capacity
	CPUWeight  float64   `json:"cpu_weight"`  // relative CPU share (cores)
	MemoryHigh int64     `json:"memory_high"` // soft limit: throttle/reclaim above (0 = none)
	MemoryMax  int64     `json:"memory_max"`  // hard limit: OOM above (0 = machine)
	Swap       int64     `json:"swap"`        // memory.swap.max (-1 = unlimited, 0 = none)
	Jobs       int       `json:"jobs"`        // build parallelism hint (0 = tool default)
}

// SlotKind is how a build slot is prepared before a session runs.
type SlotKind int

const (
	SlotNone    SlotKind = iota
	SlotPrune            // prune slots on the machine to free SlotAction.Bytes
	SlotResize           // grow the machine disk to SlotAction.Bytes
	SlotRestore          // restore SlotAction.Snapshot into the slot
	// SlotMove saves the warm slot on machine SlotAction.From (Bytes) to the
	// store and restores that fresh snapshot into the slot here.
	SlotMove
)

func (k SlotKind) String() string {
	return [...]string{"none", "prune", "resize", "restore", "move"}[k]
}

// SlotAction prepares the build slot of a placed task.
type SlotAction struct {
	Kind     SlotKind
	Repo     string
	Bytes    int64
	Snapshot Snapshot
	From     string // SlotMove: the machine holding the warm slot
}

// Placement is the decision for one task.
type Placement struct {
	Kind    PlacementKind
	Machine string
	Grant   Grant
	Reason  string
	// Slot is done on the machine before the session starts.
	Slot SlotAction
	// Expected is the expected run time (for release forecasts).
	Expected time.Duration
}

// ActionKind is a pool change.
type ActionKind int

const (
	Create  ActionKind = iota // new machine of Size for EnvHash
	Start                     // start stopped Machine
	Stop                      // stop Machine (keeps disk)
	Destroy                   // remove Machine and its disk
	Drain                     // no new sessions on Machine
	Undrain                   // accept sessions again
)

func (k ActionKind) String() string {
	return [...]string{"create", "start", "stop", "destroy", "drain", "undrain"}[k]
}

// Action is one pool change decided by Plan.
type Action struct {
	Kind    ActionKind
	Machine string // for everything but Create
	EnvHash string // for Create
	Size    Size   // for Create
	// Volume is the detached cache volume a Create attaches (empty: a
	// fresh volume for volume sizes, none for local ones).
	Volume string
	Reason string
}

// Policy makes all placement and scaling decisions. Implementations must be
// deterministic functions of their input and configuration.
type Policy interface {
	// Place decides where task runs. It is called with the task not part of
	// f.Queue.
	Place(f Pool, t Task) Placement
	// Plan returns pool changes. It is called periodically and whenever
	// tasks wait. Hard invariants (destroying expired machines, deleting
	// orphaned volumes) are enforced by the orchestrator, not the policy.
	Plan(f Pool) []Action
}

// Usage is what a finished session consumed.
type Usage struct {
	PeakMemory int64         `json:"peak_memory"`
	CPUSeconds float64       `json:"cpu_seconds"`
	Wall       time.Duration `json:"wall"`
	OOM        bool          `json:"oom"`
	Warm       bool          `json:"warm"` // build cache was reused
	ExitCode   int           `json:"exit_code"`
	// CPUStall and MemoryStall are the seconds at least one task of the
	// session waited for CPU or memory (cgroup PSI "some total"): how much
	// contention (overcommit, neighbours) slowed it down. Recorded in the
	// event log, not learned from.
	CPUStall    float64 `json:"cpu_stall_seconds,omitempty"`
	MemoryStall float64 `json:"memory_stall_seconds,omitempty"`
	// Cache holds counters of cache integrations (e.g. "sccache_hits");
	// recorded in the event log, not learned from.
	Cache map[string]int64 `json:"cache,omitempty"`
	// SlotBytesBefore and SlotBytesAfter are the size of the session's
	// build slot at its start and end (0 = unknown); the estimator learns
	// slot growth per key from them.
	SlotBytesBefore int64 `json:"slot_bytes_before,omitempty"`
	SlotBytesAfter  int64 `json:"slot_bytes_after,omitempty"`
	// Restored is set when the slot was restored from a snapshot before the
	// session ran; RestoreSeconds is how long the restore took.
	Restored       bool    `json:"restored,omitempty"`
	RestoreSeconds float64 `json:"restore_seconds,omitempty"`
	// Provider ran the session (cold durations are learned per provider).
	Provider string `json:"provider,omitempty"`
}

// Snapshot is a saved build slot of a repository in an object store.
type Snapshot struct {
	Repo     string    `json:"repo"`
	Provider string    `json:"provider"` // provider whose machine saved it
	Store    string    `json:"store"`    // store name (provider name or "storage")
	Bytes    int64     `json:"bytes"`
	At       time.Time `json:"at"`
}

// Estimator predicts the resource need of tasks from past runs.
type Estimator interface {
	// Estimate returns the expected need for key. fallback is the
	// configured value used until enough observations exist.
	Estimate(key string, fallback Resources) Resources
	// Observe records a finished run.
	Observe(key string, u Usage)
	// Profile returns what is known about the runs of key (durations,
	// slot sizes) for the cost model.
	Profile(key string) Profile
}
