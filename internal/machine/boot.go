package machine

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// BootPhase is one step of a machine's boot, reached at At (unix
// milliseconds of the host clock). Bytes is a size belonging to the step
// (the pulled image).
type BootPhase struct {
	Name  string `json:"name"`
	At    int64  `json:"at_ms"`
	Bytes int64  `json:"bytes,omitempty"`
}

// Boot phases in their usual order. The provider's boot script records
// the first ones (BootScriptPhases), the supervisor the middle ones, the
// worker the last ones.
const (
	PhaseKernel        = "kernel"         // the host kernel started (this boot)
	PhaseUserData      = "user_data"      // first boot script started
	PhaseDocker        = "docker"         // Docker installed and started by the boot script
	PhaseBinary        = "binary"         // server binary on the host
	PhaseDockerReady   = "docker_ready"   // Docker daemon answers the supervisor
	PhaseSupervisor    = "supervisor"     // supervisor process started
	PhaseCache         = "cache"          // cache medium formatted and mounted
	PhaseRegistryLogin = "registry_login" // registry credentials obtained (before a pull)
	PhaseImage         = "image"          // environment image present (Bytes: pulled size, 0: already there)
	PhaseSecrets       = "secrets"        // secret environment fetched
	PhaseContainers    = "containers"     // machine containers started
	PhaseWorker        = "worker"         // worker process started
	PhaseListening     = "listening"      // worker accepts connections
	PhaseReady         = "ready"          // worker provisioned and ready
)

// BootFile is where the supervisor publishes the boot phases inside the
// machine container (next to HostStatusFile).
const BootFile = HostStatusDir + "/boot.json"

// NowMs returns the current unix time in milliseconds.
func NowMs() int64 { return time.Now().UnixMilli() }

// WriteBoot replaces the boot phase file atomically.
func WriteBoot(path string, phases []BootPhase) error {
	b, err := json.Marshal(phases)
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// ReadBoot reads the boot phase file.
func ReadBoot(path string) ([]BootPhase, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ph []BootPhase
	err = json.Unmarshal(b, &ph)
	return ph, err
}

// ParseBootScript parses the phases a boot script recorded, one
// "<name> <unix ms>" per line; malformed lines are skipped.
func ParseBootScript(s string) []BootPhase {
	var out []BootPhase
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		ms, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || ms <= 0 {
			continue
		}
		out = append(out, BootPhase{Name: f[0], At: ms})
	}
	return out
}
