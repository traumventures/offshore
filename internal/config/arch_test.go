package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestArchAndInstanceTypeSizes(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
type = "aws-ec2"

[[providers.aws.size]]
name = "x4"
cpu = 4
memory = "8G"
price_per_hour = 0.2
instance_types = ["c7a.xlarge", "c6a.xlarge", "m7a.xlarge"]

[[providers.aws.size]]
name = "g4"
cpu = 4
memory = "8G"
price_per_hour = 0.17
arch = "arm64"
`)
	if err != nil {
		t.Fatal(err)
	}
	s := u.Pool.Sizes
	if s[0].Arch != ArchAMD64 || s[0].PricePerHour != 0.2 || len(s[0].InstanceTypes) != 3 {
		t.Errorf("amd64 size: %+v", s[0])
	}
	if s[1].Arch != ArchARM64 || s[1].PricePerHour != 0.17 {
		t.Errorf("arm64 size: %+v", s[1])
	}
}

func TestSizeValidation(t *testing.T) {
	other := ArchARM64
	if MachineArch(runtime.GOARCH) == ArchARM64 {
		other = ArchAMD64
	}
	for name, tc := range map[string]struct{ body, want string }{
		"bad arch": {`[providers.aws]
type = "aws-ec2"

[[providers.aws.size]]
name = "s"
cpu = 1
memory = "1G"
arch = "riscv64"`, "unsupported \"riscv64\""},
		"spot is gone": {`[providers.aws]
type = "aws-ec2"

[[providers.aws.size]]
name = "s"
cpu = 1
memory = "1G"
spot = true`, "spot"},
		"instance types on local docker": {`[providers.local]
type = "local-docker"

[[providers.local.size]]
name = "s"
cpu = 1
memory = "1G"
instance_types = ["x"]`, "needs an aws-ec2 or oci provider"},
		"foreign arch on local docker": {`[providers.local]
type = "local-docker"

[[providers.local.size]]
name = "s"
cpu = 1
memory = "1G"
arch = "` + other + `"`, "local-docker machines run on this host"},
	} {
		_, err := loadUserFrom(t, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	if MachineArch("aarch64") != ArchARM64 || MachineArch("x86_64") != ArchAMD64 || MachineArch("") != ArchAMD64 {
		t.Error("MachineArch")
	}
}

func TestOCIShapes(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.oci]
type = "oci"
compartment = "cloudsourced"

[[providers.oci.size]]
name = "a1-4"
cpu = 4
memory = "16G"
arch = "arm64"
instance_types = ["VM.Standard.A1.Flex"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.ProviderType("oci"); got != "oci" {
		t.Fatalf("provider type %q", got)
	}
}
