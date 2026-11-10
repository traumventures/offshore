package oci

import (
	"debug/elf"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// Machine architectures (the size naming, GOARCH).
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

func archOf(a string) string {
	if a == "" {
		return archAMD64
	}
	return a
}

// defaultShape is the flexible shape of an architecture used when a size
// lists no instance_types: AMD EPYC (E5) for amd64, Ampere A1 for arm64.
func defaultShape(arch string) string {
	if arch == archARM64 {
		return "VM.Standard.A1.Flex"
	}
	return "VM.Standard.E5.Flex"
}

// shapes returns the shapes to try for a machine in order.
func shapes(spec provider.MachineSpec) []string {
	if len(spec.InstanceTypes) > 0 {
		return spec.InstanceTypes
	}
	return []string{defaultShape(archOf(spec.Arch))}
}

// vcpusPerOCPU is the number of vCPUs (hardware threads) of one OCPU: two
// on x86 shapes (a core with SMT), one on Ampere A1 (a core without SMT).
func vcpusPerOCPU(shape string) float64 {
	if strings.Contains(shape, ".A1.") {
		return 1
	}
	return 2
}

// shapeConfig sizes a flexible shape for the machine: OCPUs from the
// size's cpu (vCPUs, see vcpusPerOCPU, rounded up) and memory in GB
// (rounded up). Fixed shapes take no configuration (nil).
func shapeConfig(shape string, cpu float64, memory int64) *core.LaunchInstanceShapeConfigDetails {
	if !strings.HasSuffix(shape, ".Flex") {
		return nil
	}
	ocpus := math.Ceil(cpu / vcpusPerOCPU(shape))
	if ocpus < 1 {
		ocpus = 1
	}
	c := &core.LaunchInstanceShapeConfigDetails{Ocpus: common.Float32(float32(ocpus))}
	if memory > 0 {
		gb := math.Ceil(float64(memory) / (1 << 30))
		c.MemoryInGBs = common.Float32(float32(gb))
	}
	return c
}

// serverBinaries returns the server binary per architecture, as on
// aws-ec2: binary_amd64 / binary_arm64 name them explicitly; binary is the
// binary of the orchestrator's architecture (default: the orchestrator's
// own executable on linux); otherwise cloudsourced-linux-<arch> next to it.
func serverBinaries(opts map[string]any, self, goos, goarch string) map[string]string {
	out := map[string]string{}
	base := provider.String(opts, "binary", "")
	if base == "" && goos == "linux" {
		base = self
	}
	if base != "" {
		out[goarch] = base
	}
	dir := filepath.Dir(self)
	if base != "" {
		dir = filepath.Dir(base)
	}
	for _, a := range []string{archAMD64, archARM64} {
		if v := provider.String(opts, "binary_"+a, ""); v != "" {
			out[a] = v
		} else if _, ok := out[a]; !ok {
			out[a] = filepath.Join(dir, "cloudsourced-linux-"+a)
		}
	}
	return out
}

// checkBinary verifies that path is a linux executable for arch.
func checkBinary(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("oci: no %s server binary at %s (build it with make build, or set binary_%s)", arch, path, arch)
		}
		return fmt.Errorf("oci: server binary %s: %w", path, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{archAMD64: elf.EM_X86_64, archARM64: elf.EM_AARCH64}[arch]
	if f.Machine != want || f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("oci: server binary %s is %s, machines of this size need linux/%s", path, f.Machine, arch)
	}
	return nil
}
