package scheduler

import (
	"math"
	"sort"
	"time"
)

// Decision explains one scheduling decision: every option the policy
// considered (with its predicted time and cost and the score it was
// ranked by), the options it rejected before scoring (with the reason)
// and the one it chose. Placements carry the decision of their task,
// starts and creates the scale-out decision that triggered them; the
// orchestrator writes them to the event log (field "decision").
type Decision struct {
	// Policy is "options" (absolute score: cost + time_value x hours) or
	// "costmodel" (cost and time normalised against the decision's
	// cheapest and fastest option).
	Policy           string  `json:"policy"`
	CostOptimization float64 `json:"cost_optimization"`
	// TimeValue is what an hour of turnaround is worth in money for the
	// option policy (+Inf as -1: only time counts).
	TimeValue float64 `json:"time_value_per_hour,omitempty"`
	// Chosen is the index of the chosen option in Options (-1: none).
	Chosen  int      `json:"chosen"`
	Options []Option `json:"options"`
	// Queue is the queue-confidence verdict: wait only when waiting
	// finishes sooner with probability >= queue_confidence (default
	// policy; see queueconf.go).
	Queue *QueueVerdict `json:"queue,omitempty"`
}

// QueueShort is the decision's queue verdict for `offshore top` ("" when
// none): "wait: p=0.86".
func (d *Decision) QueueShort() string {
	if d == nil {
		return ""
	}
	return d.Queue.Short()
}

// Option kinds.
const (
	OptAssign       = "assign"        // run now on Machine
	OptWaitMachine  = "wait_machine"  // wait for capacity on Machine (busy or starting)
	OptWaitCapacity = "wait_capacity" // scale-out: let the queued tasks wait for the capacity there or on its way
	OptStart        = "start"         // start stopped Machine
	OptCreate       = "create"        // create a machine of Provider/Size (Volume: attach a warm volume)
	OptReject       = "reject"        // the task can never run
)

// Option is one candidate of a decision. Times are seconds; TimeSeconds
// is the time to result (placement: wait + preparation + run of the task;
// scale-out: summed over the queued tasks it plans, Tasks of which the
// option takes). Cost is money (machine time, egress, extra disk).
// Rejected options were not scored (Rejected says why).
type Option struct {
	Kind     string `json:"kind"`
	Machine  string `json:"machine,omitempty"`
	Provider string `json:"provider,omitempty"`
	Size     string `json:"size,omitempty"`
	// Slot is how the build slot is prepared: warm, cold, restore, move,
	// prune, resize or combinations ("resize+restore").
	Slot   string `json:"slot,omitempty"`
	Volume string `json:"volume,omitempty"`
	Reason string `json:"reason,omitempty"`

	WaitSeconds    float64 `json:"wait_seconds,omitempty"`
	LatencySeconds float64 `json:"latency_seconds,omitempty"` // start or boot latency
	PrepSeconds    float64 `json:"prep_seconds,omitempty"`    // restore, move, prune, resize
	PageInSeconds  float64 `json:"page_in_seconds,omitempty"` // predicted thrash: refaulting a working set beyond the local tier
	RunSeconds     float64 `json:"run_seconds,omitempty"`     // preparation + run
	TimeSeconds    float64 `json:"time_seconds"`
	Tasks          int     `json:"tasks,omitempty"`
	Cost           float64 `json:"cost"`
	Score          float64 `json:"score"`
	Chosen         bool    `json:"chosen,omitempty"`
	Rejected       string  `json:"rejected,omitempty"`
}

// ChosenOption returns the chosen option (ok false when none).
func (d *Decision) ChosenOption() (Option, bool) {
	if d == nil || d.Chosen < 0 || d.Chosen >= len(d.Options) {
		return Option{}, false
	}
	return d.Options[d.Chosen], true
}

// scorer returns the score the decision's policy ranks opts by (lower is
// better): the option policy's absolute cost + timeValue x hours (choose),
// else the cost model's normalised score (pick).
func (p Default) scorer(f Pool, env string, opts []option) (score func(option) float64, policy string, lam float64) {
	if p.next() {
		if l, ok := p.timeValue(f, env); ok {
			return absScore(l), "options", l
		}
	}
	return p.pickScore(opts), "costmodel", 0
}

// absScore is the option policy's score with time value lam.
func absScore(lam float64) func(option) float64 {
	return func(o option) float64 {
		if math.IsInf(lam, 1) {
			return o.time.Seconds()
		}
		return o.cost + lam*hours(o.time)
	}
}

// decision builds the Decision of opts (scored, chosen: index or -1) and
// rejected (not scored), ordered by score with the rejected last.
func (p Default) decision(f Pool, env string, opts []option, chosen int, rejected []Option) *Decision {
	score, policy, lam := p.scorer(f, env, opts)
	d := &Decision{Policy: policy, CostOptimization: p.costOptimization(), Chosen: -1}
	if policy == "options" {
		d.TimeValue = lam
		if math.IsInf(lam, 1) {
			d.TimeValue = -1
		}
	}
	for i, o := range opts {
		var op Option
		if o.info != nil {
			op = *o.info
		}
		op.TimeSeconds, op.Cost, op.Score, op.Chosen = secs(o.time), round6(o.cost), round6(score(o)), i == chosen
		d.Options = append(d.Options, op)
	}
	sort.SliceStable(d.Options, func(i, j int) bool {
		a, b := d.Options[i], d.Options[j]
		if a.Chosen != b.Chosen {
			return a.Chosen
		}
		return a.Score < b.Score
	})
	if chosen >= 0 {
		d.Chosen = 0
	}
	d.Options = append(d.Options, rejected...)
	return d
}

func secs(d time.Duration) float64 { return math.Round(d.Seconds()*10) / 10 }

func round6(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return -1
	}
	return math.Round(v*1e6) / 1e6
}

// runOption describes running t on m with run choice rc.
func runOption(kind string, m Machine, t Task, rc runChoice, wait time.Duration) *Option {
	slot := rc.slot.Steps()
	switch rc.slot.Kind {
	case SlotNone:
		slot = "cold"
		if m.Warm(t.RepoID) {
			slot = "warm"
		}
	case SlotPrune, SlotResize:
		if m.Warm(t.RepoID) {
			slot += "+warm"
		} else {
			slot += "+cold"
		}
	}
	return &Option{Kind: kind, Machine: m.ID, Provider: m.Provider, Size: m.Size, Slot: slot, Volume: m.Volume, Reason: rc.reason,
		WaitSeconds: secs(wait), PrepSeconds: secs(rc.prep), PageInSeconds: secs(rc.thrash), RunSeconds: secs(rc.run)}
}

// rejectOption is an option of m that was not scored.
func rejectOption(kind string, m Machine, wait time.Duration, why string) Option {
	return Option{Kind: kind, Machine: m.ID, Provider: m.Provider, Size: m.Size, Volume: m.Volume, WaitSeconds: secs(wait), Rejected: why}
}

// Rejection reasons.
const (
	RejSmaller      = "machine smaller than task"
	RejQueueTimeout = "exceeds queue timeout"
	RejHedge        = "wait exceeds start/create ETA + margin"
	RejNeverFrees   = "never frees enough capacity"
	RejNotAccepting = "not accepting sessions (draining or expiring)"
	RejNoTasks      = "takes none of the queued tasks"
	RejOverdue      = "queued longer than a start or create takes"
	RejNoViolation  = "no queued request would wait longer than the machine takes"
	// Queue confidence (queueconf.go).
	RejConfidentWait = "every queued request it could serve is confidently better off waiting"
	RejNotConfident  = "not confident that waiting finishes every queued request sooner"
	RejProviderDown  = "provider down (capacity/quota cooldown or disabled)"
	RejCreateBlock   = "create backoff after a failed create"
	RejMachineLimit  = "provider limit, disabled provider or max_spend_per_hour"
	RejGuard         = "runaway guard reached (no request waits longer than a create takes)"
)

// sizeRejections lists the sizes of env that could not be created now,
// with the reason.
func (p Default) sizeRejections(st *planState, env string, allowed []Size) []Option {
	f := st.f
	ok := map[string]bool{}
	for _, s := range allowed {
		ok[s.Provider+"/"+s.Name] = true
	}
	var out []Option
	for _, s := range f.ArchSizes(env) {
		if ok[s.Provider+"/"+s.Name] {
			continue
		}
		why := RejMachineLimit
		switch {
		case providerDown(f, s.Provider):
			why = RejProviderDown
		case !f.CanCreate(env, s.Provider):
			why = RejCreateBlock
		}
		out = append(out, Option{Kind: OptCreate, Provider: s.Provider, Size: s.Name, Rejected: why})
	}
	return out
}
