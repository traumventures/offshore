package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// HostStatus is what the server publishes for the supervisor, refreshed
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

// Decision is what the supervisor should do with its machine.
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

// Lifecycle parameters of the supervisor's decision.
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

// Decide is the supervisor's lifecycle decision. st is nil when no
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
