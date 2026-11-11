// Package scheduler contains the scheduling decisions of cloudsourced as
// pure functions over a pool snapshot: where a task runs (Place), how the
// pool changes (Plan) and how much a task needs (Estimator).
//
// The orchestrator gathers state, calls a Policy and executes its
// decisions; it contains no placement or scaling logic itself. Policies can
// therefore be swapped, tuned and tested (or simulated) without machines.
package scheduler

import (
	"time"
)

// Resources is an amount of CPU (cores) and memory (bytes).
type Resources struct {
	CPU    float64 `json:"cpu"`
	Memory int64   `json:"memory"`
}

// Add returns r + o.
func (r Resources) Add(o Resources) Resources {
	return Resources{CPU: r.CPU + o.CPU, Memory: r.Memory + o.Memory}
}

// Sub returns r - o.
func (r Resources) Sub(o Resources) Resources {
	return Resources{CPU: r.CPU - o.CPU, Memory: r.Memory - o.Memory}
}

// Fits reports whether o fits into r.
func (r Resources) Fits(o Resources) bool { return o.CPU <= r.CPU && o.Memory <= r.Memory }

// Scale multiplies CPU and memory by separate factors.
func (r Resources) Scale(cpu, mem float64) Resources {
	return Resources{CPU: r.CPU * cpu, Memory: int64(float64(r.Memory) * mem)}
}

// Size is a machine type a backend can create.
type Size struct {
	Provider     string    `json:"provider"` // provider instance that creates it
	Name         string    `json:"name"`
	Capacity     Resources `json:"capacity"`
	PricePerHour float64   `json:"price_per_hour"` // 0 when unknown
	// Arch is the machine architecture ("amd64", "arm64"; empty: amd64).
	// Machines are only created for environments of their architecture.
	Arch string `json:"arch,omitempty"`
	// InstanceTypes are passed to the provider: machine types providing
	// this size, tried in order on capacity errors.
	InstanceTypes []string `json:"instance_types,omitempty"`
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
	WarmRepos []string  // repositories with an unexpired build cache
	IdleSince time.Time // zero while sessions run
	CreatedAt time.Time
	ExpiresAt time.Time
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
}

// SizesFor returns the sizes whose architecture matches env's.
func (f Pool) SizesFor(env string) []Size {
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

// Placement is the decision for one task.
type Placement struct {
	Kind    PlacementKind
	Machine string
	Grant   Grant
	Reason  string
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
	Reason  string
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
}

// Estimator predicts the resource need of tasks from past runs.
type Estimator interface {
	// Estimate returns the expected need for key. fallback is the
	// configured value used until enough observations exist.
	Estimate(key string, fallback Resources) Resources
	// Observe records a finished run.
	Observe(key string, u Usage)
}
