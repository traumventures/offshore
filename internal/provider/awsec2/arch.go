package awsec2

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"

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

// serverBinaries returns the server binary per architecture. Machines run
// the static linux build of their architecture (the host agent and the
// server in the machine container are the same binary):
//
//   - binary_amd64 / binary_arm64 options name them explicitly;
//   - binary (legacy) is the binary of the orchestrator's architecture,
//     default: the orchestrator's own executable when it runs on linux;
//   - otherwise cloudsourced-linux-<arch> next to that binary (make build
//     puts bin/cloudsourced-linux-arm64 next to bin/cloudsourced).
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

// checkBinary verifies that path is a linux executable for arch, so a
// machine never boots a binary it cannot run.
func checkBinary(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("aws-ec2: no %s server binary at %s (build it with make build, or set binary_%s)", arch, path, arch)
		}
		return fmt.Errorf("aws-ec2: server binary %s: %w", path, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{archAMD64: elf.EM_X86_64, archARM64: elf.EM_AARCH64}[arch]
	if f.Machine != want || f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("aws-ec2: server binary %s is %s, machines of this size need linux/%s", path, f.Machine, arch)
	}
	return nil
}

// amiParameter is the public SSM parameter of the latest Amazon Linux 2023
// AMI for arch.
func amiParameter(arch string) string {
	suffix := "x86_64"
	if arch == archARM64 {
		suffix = "arm64"
	}
	return "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-" + suffix
}

// imagePlatform is the container platform of an environment image.
func imagePlatform(img provider.ImageSpec) string {
	if img.Platform != "" {
		return img.Platform
	}
	return "linux/amd64"
}
