package medium

import (
	"math"
	"testing"
	"time"
)

// transfer adds n requests of alternating sizes taking lat + bytes/tp.
func transfer(r *Recorder, med string, write bool, n int, lat time.Duration, tp float64) {
	for i := range n {
		b := int64(64<<10) << (i % 6) // 64 KiB .. 2 MiB
		r.Add(med, write, b, lat.Seconds()+float64(b)/tp)
	}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol*math.Abs(b) }

func TestFitEstimate(t *testing.T) {
	var r Recorder
	transfer(&r, Store, false, 60, 30*time.Millisecond, 200e6)
	c := r.Snapshot()
	if len(c) != 1 || c[0].Medium != Store {
		t.Fatalf("%+v", c)
	}
	tp, lat := c[0].Reads.Estimate()
	if !near(float64(tp), 200e6, 0.01) || !near(lat.Seconds(), 0.03, 0.01) {
		t.Errorf("tp %d lat %v", tp, lat)
	}
	// equal sizes: no latency, plain ratio
	var f Fit
	for range 10 {
		f.Add(1<<20, 0.01)
	}
	if tp, lat := f.Estimate(); lat != 0 || !near(float64(tp), 1<<20/0.01, 0.001) {
		t.Errorf("uniform: %d %v", tp, lat)
	}
	if tp, lat := (Fit{}).Estimate(); tp != 0 || lat != 0 {
		t.Error("empty fit")
	}
	r.Add("", false, 1, 1) // no medium: ignored
	r.Add(Store, false, 0, 1)
	if got := r.Snapshot(); got[0].Reads.N != 60 {
		t.Errorf("ignored samples counted: %v", got[0].Reads.N)
	}
	if MediumOf("volume>store") != Volume || MediumOf("store") != Store || MediumOf("files") != "" {
		t.Error("MediumOf")
	}
}

func TestMeter(t *testing.T) {
	m := NewMeter()
	m.MinRequests = 20
	now := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return now }
	var a, b Recorder
	transfer(&a, Store, false, 12, 30*time.Millisecond, 100e6)
	m.Update("m1", "aws", a.Snapshot())
	if _, ok := m.Profile("aws", Store); ok {
		t.Fatal("below MinRequests")
	}
	transfer(&b, Store, false, 12, 30*time.Millisecond, 100e6)
	m.Update("m2", "aws", b.Snapshot())
	m.Update("m2", "aws", b.Snapshot()) // same counters again: no delta
	p, ok := m.Profile("aws", Store)
	if !ok || !near(float64(p.Throughput), 100e6, 0.01) || !near(p.Latency.Seconds(), 0.03, 0.02) || p.Medium != Store {
		t.Fatalf("profile %+v %v", p, ok)
	}
	if _, ok := m.Profile("oci", Store); ok {
		t.Error("other provider")
	}
	// later the store got faster; old samples decay away
	now = now.Add(5 * time.Hour)
	transfer(&a, Store, false, 60, 30*time.Millisecond, 400e6)
	m.Update("m1", "aws", a.Snapshot())
	p, _ = m.Profile("aws", Store)
	if !near(float64(p.Throughput), 400e6, 0.1) {
		t.Errorf("decayed throughput %d", p.Throughput)
	}
	// a restarted worker (counters backwards) counts from zero
	var fresh Recorder
	transfer(&fresh, Volume, true, 30, 0, 50e6)
	m.Forget("m1")
	m.Update("m1", "aws", fresh.Snapshot())
	if p, ok := m.Profile("aws", Volume); !ok || !near(float64(p.Throughput), 50e6, 0.01) {
		t.Errorf("write-only profile %+v", p)
	}
}

func TestResolveMeasured(t *testing.T) {
	m := NewMeter()
	m.MinRequests = 1
	var r Recorder
	transfer(&r, Store, false, 12, 20*time.Millisecond, 300e6)
	m.Update("m1", "prod", r.Snapshot())
	SetMeasured(m.Profile)
	defer SetMeasured(nil)
	p := Resolve("aws-ec2", "prod", Store, Profile{})
	if p.Source != "measured" || !near(float64(p.Throughput), 300e6, 0.01) || p.PricePerGBMonth != 0.023 {
		t.Errorf("%+v", p)
	}
	if p := Resolve("aws-ec2", "other", Store, Profile{}); p.Source != "default" {
		t.Errorf("unmeasured: %+v", p)
	}
}
