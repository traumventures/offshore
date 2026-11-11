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
