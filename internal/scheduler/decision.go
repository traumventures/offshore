package scheduler

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Decision explains one scheduling decision: every option the policy
// considered (with its predicted time and cost and the score it was
// ranked by), the options it rejected before scoring (with the reason)
// and the one it chose. Placements carry the decision of their task,
// starts and creates the scale-out decision that triggered them; the
// orchestrator writes them to the event log (field "decision").
type Decision struct {
	// Policy is "v2" (score: cost + time_value x hours).
	Policy           string  `json:"policy"`
	CostOptimization float64 `json:"cost_optimization"`
	// TimeValue is what an hour of turnaround is worth in money for the
	// option policy (+Inf as -1: only time counts).
	TimeValue float64 `json:"time_value_per_hour,omitempty"`
	// Chosen is the index of the chosen option in Options (-1: none).
	Chosen  int      `json:"chosen"`
	Options []Option `json:"options"`
}

// QueueShort is the chosen option of a waiting request for `offshore
// top` ("" when none): "wait 2m on m-1", "start m-2", "create aws/x8".
func (d *Decision) QueueShort() string {
	o, ok := d.ChosenOption()
	if !ok {
		return ""
	}
	switch o.Kind {
	case OptWaitMachine:
		return fmt.Sprintf("wait %s on %s", (time.Duration(o.WaitSeconds) * time.Second).String(), o.Machine)
	case OptStart:
		return "start " + o.Machine
	case OptCreate:
		return "create " + o.Provider + "/" + o.Size
	}
	return o.Kind
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
	if rc.slot.Kind == SlotNone {
		w := "cold"
		if m.Warm(t.RepoID) {
			w = "warm"
		}
		if rc.slot.None() {
			slot = w
		} else {
			slot = strings.TrimSuffix(slot, "none") + w
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
