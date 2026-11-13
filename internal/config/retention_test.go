package config

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/medium"
)

func TestStorageRetention(t *testing.T) {
	f, err := loadUserFrom(t, "[pool]\nid = \"p\"\n")
	if err != nil || f.Storage.RetentionDuration() != 24*time.Hour {
		t.Fatalf("default: %v %v", f, err)
	}
	// Retention alone (native stores) needs no bucket and is kept.
	f, err = loadUserFrom(t, "[storage]\nretention = \"3d\"\n")
	if err != nil || f.Storage.RetentionDuration() != 72*time.Hour || f.Storage.Configured() {
		t.Fatalf("3d: %+v %v", f.Storage, err)
	}
	if doc, err := f.Document(); err != nil || doc["storage"] == nil {
		t.Fatalf("document %v %v", doc, err)
	}
	if f, err = loadUserFrom(t, "[storage]\ndir = \"/s\"\nretention = \"7d\"\n"); err != nil || f.Storage.RetentionDuration() != 7*24*time.Hour {
		t.Fatalf("7d: %v", err)
	}
	for _, bad := range []string{"12h", "8d", "soon"} {
		if _, err := loadUserFrom(t, "[storage]\nretention = \""+bad+"\"\n"); err == nil || !strings.Contains(err.Error(), "storage.retention") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// The aws-ec2 gp3 root volume is priced with its provisioned IOPS and
// throughput above the baseline, pro rata over disk_gb.
func TestRootDiskProfilePrice(t *testing.T) {
	u, err := loadUserFrom(t, `
[providers.aws]
type = "aws-ec2"
disk_gb = 200
disk_iops = 6000
disk_throughput = 500
[providers.dflt]
type = "aws-ec2"
[providers.base]
type = "aws-ec2"
disk_iops = 3000
disk_throughput = 125
`)
	if err != nil {
		t.Fatal(err)
	}
	// (3000*0.005 + 375*0.04) / 200 GB = 30/200.
	l := u.Profile("aws", medium.Local)
	if math.Abs(l.PricePerGBMonth-(0.08+30.0/200)) > 1e-9 || l.IOPS != 6000 || l.Throughput != 500<<20 {
		t.Fatalf("aws local %+v", l)
	}
	// Defaults: 100 GB, 6000 IOPS, 500 MiB/s.
	if d := u.Profile("dflt", medium.Local); math.Abs(d.PricePerGBMonth-(0.08+30.0/100)) > 1e-9 {
		t.Fatalf("default local %+v", d)
	}
	if b := u.Profile("base", medium.Local); math.Abs(b.PricePerGBMonth-0.08) > 1e-9 {
		t.Fatalf("baseline local %+v", b)
	}
	// Other media keep their prices.
	if v := u.Profile("aws", medium.Volume); v.PricePerGBMonth != 0.08 {
		t.Fatalf("volume %+v", v)
	}
	if n := u.Profile("aws", medium.LocalNVMe); n.PricePerGBMonth != 0 {
		t.Fatalf("nvme %+v", n)
	}
}
