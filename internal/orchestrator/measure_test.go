package orchestrator

import (
	"context"
	"io"
	"testing"

	"github.com/traumventures/offshore/internal/medium"
)

func TestMeasuredTierProfiles(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	defer medium.SetMeasured(nil)
	o.meter.MinRequests = 1
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var r medium.Recorder
	for i := range 12 {
		b := int64(64<<10) << (i % 4)
		r.Add(medium.Store, false, b, 0.02+float64(b)/150e6)
	}
	f.mu.Lock()
	f.status[l.MachineID].Tiers = r.Snapshot()
	f.mu.Unlock()
	o.Sample(context.Background())
	o.mu.Lock()
	sizes := o.measuredSizes()
	o.mu.Unlock()
	st := sizes[0].Stack.Store
	if st.Source != "measured" || st.Throughput < 140e6 || st.Throughput > 160e6 || st.Latency <= 0 {
		t.Fatalf("store profile %+v", st)
	}
	if o.cfg.Sizes[0].Stack.Store.Source == "measured" {
		t.Error("configured sizes must stay unchanged")
	}
	if p := medium.Resolve("aws-ec2", "fake", medium.Store, medium.Profile{}); p.Source != "measured" {
		t.Errorf("Resolve: %+v", p)
	}
}
