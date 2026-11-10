package awsec2

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/traumventures/cloudsourced/internal/provider"
)

func TestServerBinaries(t *testing.T) {
	b := serverBinaries(map[string]any{}, "/opt/cs/bin/cloudsourced", "linux", "amd64")
	if b["amd64"] != "/opt/cs/bin/cloudsourced" || b["arm64"] != "/opt/cs/bin/cloudsourced-linux-arm64" {
		t.Errorf("defaults: %v", b)
	}
	b = serverBinaries(map[string]any{"binary": "/x/cs", "binary_arm64": "/y/cs-arm"}, "/opt/cs", "linux", "amd64")
	if b["amd64"] != "/x/cs" || b["arm64"] != "/y/cs-arm" {
		t.Errorf("explicit: %v", b)
	}
	// An orchestrator on a Mac hands out the linux builds next to it.
	b = serverBinaries(map[string]any{}, "/usr/local/bin/cloudsourced", "darwin", "arm64")
	if b["amd64"] != "/usr/local/bin/cloudsourced-linux-amd64" || b["arm64"] != "/usr/local/bin/cloudsourced-linux-arm64" {
		t.Errorf("darwin: %v", b)
	}
}

func TestCheckBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		t.Skip("needs a linux test binary")
	}
	if err := checkBinary(self, runtime.GOARCH); err != nil {
		t.Errorf("own binary: %v", err)
	}
	other := archARM64
	if runtime.GOARCH == archARM64 {
		other = archAMD64
	}
	if err := checkBinary(self, other); err == nil || !strings.Contains(err.Error(), "need linux/"+other) {
		t.Errorf("wrong architecture accepted: %v", err)
	}
	if err := checkBinary(filepath.Join(t.TempDir(), "missing"), other); err == nil || !strings.Contains(err.Error(), "binary_"+other) {
		t.Errorf("missing binary: %v", err)
	}
}

func TestArchSelection(t *testing.T) {
	if p := amiParameter(archAMD64); !strings.HasSuffix(p, "al2023-ami-kernel-default-x86_64") {
		t.Error(p)
	}
	if p := amiParameter(archARM64); !strings.HasSuffix(p, "al2023-ami-kernel-default-arm64") {
		t.Error(p)
	}
	if k := binaryKey(archARM64, []byte("x")); !strings.HasPrefix(k, "bin/cloudsourced-linux-arm64-") || k == binaryKey(archAMD64, []byte("x")) {
		t.Error(k)
	}
	if got := machineImage("r", provider.MachineSpec{Image: "cloudsourced-env:abc", Arch: archARM64}); got != "r:abc-arm64" {
		t.Errorf("arm64 machine image %s", got)
	}
	if got := machineImage("r", provider.MachineSpec{Image: "cloudsourced-env:abc"}); got != "r:abc-amd64" {
		t.Errorf("amd64 machine image %s", got)
	}
	if archOf("") != archAMD64 {
		t.Error("default arch")
	}
}
