package medium

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	for _, typ := range []string{"aws-ec2", "oci", "local-docker"} {
		for _, m := range []string{Local, LocalNVMe, Volume, Store} {
			p := Default(typ, m)
			if p.Medium == "" || p.Source != "default" || p.Throughput <= 0 {
				t.Errorf("%s %s: %+v", typ, m, p)
			}
		}
	}
	if p := Default("static", Volume); p.Medium != "" {
		t.Errorf("static has no volumes: %+v", p)
	}
	if v := Default("aws-ec2", Volume); v.Reach != ReachZone || v.ResizeCooldown != 6*time.Hour || v.Attach <= 0 {
		t.Errorf("ebs: %+v", v)
	}
	if Default("aws-ec2", LocalNVMe).Medium != Local {
		t.Error("instance store is medium local")
	}
}

func TestResolveLayers(t *testing.T) {
	defer SetMeasured(nil)
	p := Resolve("aws-ec2", "aws", Volume, Profile{PricePerGBMonth: 0.1, Attach: 20 * time.Second})
	if p.PricePerGBMonth != 0.1 || p.Attach != 20*time.Second || p.IOPS != 3000 || p.Source != "config" || p.Reach != ReachZone {
		t.Fatalf("config: %+v", p)
	}
	SetMeasured(func(prov, med string) (Profile, bool) {
		return Profile{Throughput: 300 << 20}, prov == "aws" && med == Volume
	})
	p = Resolve("aws-ec2", "aws", Volume, Profile{})
	if p.Throughput != 300<<20 || p.Source != "measured" {
		t.Fatalf("measured: %+v", p)
	}
	if p := Resolve("aws-ec2", "other", Volume, Profile{}); p.Source != "default" {
		t.Fatalf("other provider: %+v", p)
	}
}

func TestStack(t *testing.T) {
	s := Stack{Hot: Default("oci", Volume), Store: Default("oci", Store)}
	// 48 MB/s volume is slower than the 200 MB/s store.
	if got := s.RestoreTime(480 << 20); got != 10*time.Second {
		t.Fatalf("restore %v", got)
	}
	if (Stack{}).Medium() != Local {
		t.Fatal("default medium")
	}
}
