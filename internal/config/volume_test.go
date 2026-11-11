package config

import (
	"slices"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

func TestVolumeKeys(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
type = "aws-ec2"
[providers.aws.volume]
iops = 4000
throughput = 250
min = "200G"
max = "1T"
step = "100G"
keep_detached = true
ttl = "12h"
[providers.aws.media.volume]
attach = "20s"
price_per_gb_month = 0.1
[providers.aws.media.store]
throughput = "500M"
[[providers.aws.size]]
name = "v"
cpu = 4
memory = "8G"
medium = "volume"
[[providers.aws.size]]
name = "l"
cpu = 4
memory = "8G"
`)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Volumes["aws"]
	if !v.KeepDetached || v.IOPS != 4000 || v.Throughput != 250 {
		t.Fatalf("volume %+v", v)
	}
	minB, maxB, step, ttl := v.VolumeSizes(0)
	if minB != 200<<30 || maxB != 1<<40 || step != 100<<30 || ttl != 12*time.Hour {
		t.Fatalf("sizes %d %d %d %v", minB, maxB, step, ttl)
	}
	if u.Pool.Sizes[0].Medium != MediumVolume || u.Pool.Sizes[1].Medium != "" {
		t.Fatalf("media %+v", u.Pool.Sizes)
	}
	if _, ok := u.Providers["aws"]["volume"]; ok {
		t.Fatal("volume table passed as a provider option")
	}
	p := u.Profile("aws", medium.Volume)
	if p.Attach != 20*time.Second || p.PricePerGBMonth != 0.1 || p.IOPS != 3000 || p.Source != "config" {
		t.Fatalf("profile %+v", p)
	}
	if s := u.Profile("aws", medium.Store); s.Throughput != 500<<20 || s.PricePerGBMonth != 0.023 {
		t.Fatalf("store %+v", s)
	}
	if l := u.Profile("aws", medium.LocalNVMe); l.Source != "default" || l.Throughput < 1<<30 {
		t.Fatalf("nvme %+v", l)
	}
	d := VolumeConfig{}
	if minB, maxB, step, ttl := d.VolumeSizes(300 << 30); minB != 300<<30 || maxB != 0 || step != 50<<30 || ttl != 24*time.Hour {
		t.Fatalf("defaults %d %d %d %v", minB, maxB, step, ttl)
	}
	for _, bad := range []string{
		"[providers.s]\ntype = \"static\"\nhosts = [\"h\"]\n[[providers.s.size]]\nname = \"x\"\ncpu = 1\nmemory = \"1G\"\nmedium = \"volume\"\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[[providers.a.size]]\nname = \"x\"\ncpu = 1\nmemory = \"1G\"\nmedium = \"nvme\"\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[providers.a.volume]\nmin = \"2T\"\nmax = \"1T\"\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[providers.a.volume]\nttl = \"soon\"\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[providers.a.volume]\nsize = \"1T\"\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[providers.a.media.ram]\niops = 1\n",
		"[providers.a]\ntype = \"aws-ec2\"\n[providers.a.media.volume]\nattach = \"quick\"\n",
	} {
		if _, err := loadUserFrom(t, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSizeStack(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
type = "aws-ec2"
[[providers.aws.size]]
name = "lvs"
cpu = 4
memory = "8G"
stack = ["local", "volume", "store"]
[[providers.aws.size]]
name = "v"
cpu = 4
memory = "8G"
medium = "volume"
[[providers.aws.size]]
name = "l"
cpu = 4
memory = "8G"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"local", "volume", "store"}, {"volume", "store"}, {"local", "store"}}
	for i, sz := range u.Pool.Sizes {
		if got := sz.TierStack(); !slices.Equal(got, want[i]) {
			t.Errorf("size %s: stack %v, want %v", sz.Name, got, want[i])
		}
		if sz.HasVolume() != (i < 2) {
			t.Errorf("size %s: volume %v", sz.Name, sz.HasVolume())
		}
	}
	size := "[providers.a]\ntype = \"aws-ec2\"\n[[providers.a.size]]\nname = \"x\"\ncpu = 1\nmemory = \"1G\"\n"
	for _, bad := range []string{
		size + "stack = [\"volume\", \"local\", \"store\"]\n",
		size + "stack = [\"local\", \"volume\"]\n",
		size + "stack = [\"store\"]\n",
		size + "stack = [\"local\", \"local\", \"store\"]\n",
		size + "stack = [\"ram\", \"store\"]\n",
		size + "medium = \"volume\"\nstack = [\"local\", \"store\"]\n",
		size + "medium = \"local\"\nstack = [\"local\", \"volume\", \"store\"]\n",
		"[providers.s]\ntype = \"static\"\nhosts = [\"h\"]\n[[providers.s.size]]\nname = \"x\"\ncpu = 1\nmemory = \"1G\"\nstack = [\"local\", \"volume\", \"store\"]\n",
	} {
		if _, err := loadUserFrom(t, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The credentials profile option of aws-ec2/oci ("profile") is a plain
// provider option and must not collide with the media tables.
func TestProviderProfileOptionWithMedia(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
type = "aws-ec2"
profile = "ops"
[providers.aws.media.store]
throughput = "500M"
`)
	if err != nil {
		t.Fatal(err)
	}
	if u.Providers["aws"]["profile"] != "ops" {
		t.Fatalf("options %+v", u.Providers["aws"])
	}
	if s := u.Profile("aws", medium.Store); s.Throughput != 500<<20 {
		t.Fatalf("store %+v", s)
	}
}
