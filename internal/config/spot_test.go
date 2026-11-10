package config

import (
	"math"
	"runtime"
	"strings"
	"testing"
)

func TestSpotAndArchSizes(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
backend = "aws-ec2"

[[fleet.size]]
name = "x4-spot"
cpu = 4
memory = "8G"
price_per_hour = 0.2
spot = true
instance_types = ["c7a.xlarge", "c6a.xlarge", "m7a.xlarge"]

[[fleet.size]]
name = "g4"
cpu = 4
memory = "8G"
price_per_hour = 0.17
arch = "arm64"
spot = true
spot_discount = 0.5
spot_fallback = false

[[fleet.size]]
name = "c7a.xlarge"
cpu = 4
memory = "8G"
price_per_hour = 0.2
`)
	if err != nil {
		t.Fatal(err)
	}
	s := u.Fleet.Sizes
	if s[0].Arch != ArchAMD64 || !s[0].Spot || !s[0].Fallback() || math.Abs(s[0].EffectivePrice()-0.08) > 1e-9 || len(s[0].InstanceTypes) != 3 {
		t.Errorf("spot size: %+v (price %v)", s[0], s[0].EffectivePrice())
	}
	if s[1].Arch != ArchARM64 || s[1].Fallback() || math.Abs(s[1].EffectivePrice()-0.085) > 1e-9 {
		t.Errorf("arm64 size: %+v (price %v)", s[1], s[1].EffectivePrice())
	}
	if s[2].Spot || s[2].EffectivePrice() != 0.2 || s[2].Discount() != 0 {
		t.Errorf("on-demand size: %+v", s[2])
	}
}

func TestSpotAndArchValidation(t *testing.T) {
	other := ArchARM64
	if MachineArch(runtime.GOARCH) == ArchARM64 {
		other = ArchAMD64
	}
	for name, tc := range map[string]struct{ body, want string }{
		"bad arch": {`[providers.aws]
backend = "aws-ec2"
[[fleet.size]]
name = "s"
cpu = 1
memory = "1G"
arch = "riscv64"`, "unsupported \"riscv64\""},
		"bad discount": {`[providers.aws]
backend = "aws-ec2"
[[fleet.size]]
name = "s"
cpu = 1
memory = "1G"
spot = true
spot_discount = 1.5`, "spot_discount"},
		"spot on local docker": {`[providers.local]
backend = "local-docker"
[[fleet.size]]
name = "s"
cpu = 1
memory = "1G"
spot = true`, "need an aws-ec2 provider"},
		"foreign arch on local docker": {`[providers.local]
backend = "local-docker"
[[fleet.size]]
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
