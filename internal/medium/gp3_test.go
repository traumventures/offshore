package medium

import (
	"math"
	"testing"
)

func TestGP3PricePerGBMonthOf(t *testing.T) {
	for _, c := range []struct {
		gb, iops, tp int64
		want         float64
	}{
		{100, 3000, 125, 0.08},
		{100, 6000, 500, 0.08 + 0.30},   // 15 $ IOPS + 15 $ throughput
		{200, 6000, 500, 0.08 + 0.15},   // the same extra over twice the size
		{1000, 16000, 1000, 0.08 + 0.1}, // 65 + 35 = 100 $ over 1000 GB
		{0, 6000, 500, 0.08},
	} {
		if got := GP3PricePerGBMonthOf(c.gb, c.iops, c.tp); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%+v: %v, want %v", c, got, c.want)
		}
	}
	p := GP3Profile(Default("aws-ec2", Local), 200, 0, 0)
	if p.IOPS != 3000 || p.Throughput != 125<<20 || p.PricePerGBMonth != 0.08 {
		t.Fatalf("baseline profile %+v", p)
	}
}
