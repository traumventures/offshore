package awsec2

import "testing"

func TestCheckGP3(t *testing.T) {
	if err := checkGP3(200, DefaultDiskIOPS, DefaultDiskThroughput); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if err := checkGP3(100, 3000, 125); err != nil {
		t.Fatalf("gp3 baseline: %v", err)
	}
	for _, c := range []struct{ gb, iops, tp int32 }{
		{200, 2000, 125},  // below the baseline IOPS
		{200, 3000, 100},  // below the baseline throughput
		{200, 3000, 1000}, // throughput above a quarter of the IOPS
		{8, 6000, 500},    // more than 500 IOPS per GiB
		{200, 6000, 2500}, // throughput above the gp3 maximum
	} {
		if checkGP3(c.gb, c.iops, c.tp) == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}
