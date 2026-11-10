package fleet

import (
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
)

// Images are built for the architecture of the size a machine is created
// with (fleet.size arch), not for the provider's default platform.
func TestSizeArchSelectsImagePlatform(t *testing.T) {
	l := &Local{cfg: &config.FleetConfig{Fleet: config.Fleet{Sizes: []config.Size{
		{Provider: "aws", Name: "x4", Arch: "amd64"},
		{Provider: "aws", Name: "g4", Arch: "arm64"},
	}}}}
	if a := l.sizeArch("aws", "g4"); a != "arm64" {
		t.Errorf("g4: %q", a)
	}
	if a := l.sizeArch("aws", "x4"); a != "amd64" {
		t.Errorf("x4: %q", a)
	}
	if a := l.sizeArch("other", "g4"); a != "" {
		t.Errorf("unknown size: %q", a)
	}
}
