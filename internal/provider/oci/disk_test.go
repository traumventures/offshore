package oci

import "testing"

func TestCheckVPUs(t *testing.T) {
	for _, v := range []int64{10, DefaultDiskVPUs, 30, 120} {
		if err := checkVPUs(v); err != nil {
			t.Errorf("%d: %v", v, err)
		}
	}
	for _, v := range []int64{0, 5, 15, 130} {
		if checkVPUs(v) == nil {
			t.Errorf("%d accepted", v)
		}
	}
}

func TestLifecycleRules(t *testing.T) {
	r := lifecycleRules(3)
	if len(r) != 2 || *r[0].TimeAmount != 3 || r[0].ObjectNameFilter.InclusionPrefixes[0] != "cache/" ||
		*r[1].TimeAmount != 1 || r[1].ObjectNameFilter.InclusionPrefixes[0] != "secrets/" {
		t.Fatalf("rules %+v", r)
	}
	if r := lifecycleRules(0); *r[0].TimeAmount != 1 {
		t.Fatalf("default retention %d", *r[0].TimeAmount)
	}
}
