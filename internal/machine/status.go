package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// HostStatus is what the server publishes for the host agent, refreshed
// every few seconds. Times are unix seconds (0: unset).
type HostStatus struct {
	UpdatedAt int64 `json:"updated_at"`
	IdleSince int64 `json:"idle_since"` // 0 while sessions run
	LastProbe int64 `json:"last_probe"` // last status request of an orchestrator (server start if none yet)
	Sessions  int   `json:"sessions"`
	Ready     bool  `json:"ready"`
}

// WriteHostStatus replaces the status file atomically.
func WriteHostStatus(path string, st HostStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".status-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), path)
}

// ReadHostStatus reads the status file.
func ReadHostStatus(path string) (HostStatus, error) {
	var st HostStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// Decision is what the host agent should do with its machine.
type Decision int

const (
	Keep      Decision = iota
	Terminate          // lifetime over: shut down (instances terminate on shutdown)
	IdleStop           // stop the machine, keeping its disk
)

func (d Decision) String() string {
	switch d {
	case Terminate:
		return "terminate"
	case IdleStop:
		return "idle-stop"
	}
	return "keep"
}

// Lifecycle parameters of the host agent's decision.
const (
	// MinUnobserved is the minimum time without an orchestrator probe
	// before a machine stops itself; a running control plane keeps deciding.
	MinUnobserved = 10 * time.Minute
	// StatusStale: older status files are ignored (server down or starting).
	StatusStale = 2 * time.Minute
	// ExpiryGrace lets sessions still running at expiry finish (the server
	// drains at expiry and gives up 10 minutes later).
	ExpiryGrace = 10 * time.Minute
)

// UnobservedWindow is how long no orchestrator may have probed a machine
// before it stops itself: max(idleStop, MinUnobserved).
func UnobservedWindow(idleStop time.Duration) time.Duration {
	return max(idleStop, MinUnobserved)
}

// Decide is the host agent's lifecycle decision. st is nil when no
// current status is available.
func Decide(now, expiresAt time.Time, idleStop time.Duration, st *HostStatus) Decision {
	if st != nil && now.Sub(time.Unix(st.UpdatedAt, 0)) > StatusStale {
		st = nil
	}
	if !expiresAt.IsZero() && !now.Before(expiresAt) {
		if st == nil || st.Sessions == 0 || !now.Before(expiresAt.Add(ExpiryGrace)) {
			return Terminate
		}
		return Keep
	}
	if idleStop <= 0 || st == nil || !st.Ready || st.Sessions > 0 || st.IdleSince == 0 {
		return Keep
	}
	if now.Sub(time.Unix(st.IdleSince, 0)) < idleStop {
		return Keep
	}
	if now.Sub(time.Unix(st.LastProbe, 0)) < UnobservedWindow(idleStop) {
		return Keep
	}
	return IdleStop
}

// DrainNoticeFile is the file (next to the status file, in the host status
// directory) through which the host agent tells the server that the machine
// is about to be taken away, e.g. by a spot interruption. The server then
// drains: no new sessions, running sessions continue until the deadline.
const DrainNoticeFile = "drain.json"

// Drain reasons of a DrainNotice.
const (
	DrainSpotInterruption = "spot-interruption"
	DrainSpotRebalance    = "spot-rebalance"
)

// DrainNotice is a host's notice that its machine goes away.
type DrainNotice struct {
	Reason   string `json:"reason"`
	Deadline int64  `json:"deadline"` // unix seconds the machine is taken away (0: unknown)
	Action   string `json:"action,omitempty"`
	Noticed  int64  `json:"noticed"` // unix seconds the host noticed it
}

// WriteDrainNotice replaces the drain notice in dir atomically.
func WriteDrainNotice(dir string, n DrainNotice) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".drain-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), filepath.Join(dir, DrainNoticeFile))
}

// ReadDrainNotice reads the drain notice in dir; ok is false when there is
// none.
func ReadDrainNotice(dir string) (n DrainNotice, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, DrainNoticeFile))
	if os.IsNotExist(err) {
		return n, false, nil
	}
	if err != nil {
		return n, false, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, false, err
	}
	return n, true, nil
}
