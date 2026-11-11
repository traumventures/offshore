package config

import "testing"

func TestParseDiskBudget(t *testing.T) {
	b, err := ParseDiskBudget("80%")
	if err != nil || b.Of(1000) != 800 {
		t.Fatalf("80%%: %+v %v", b, err)
	}
	b, err = ParseDiskBudget("150G")
	if err != nil || b.Of(1<<40) != 150<<30 || b.Of(100<<30) != 100<<30 {
		t.Fatalf("150G: %+v %v", b, err)
	}
	for _, bad := range []string{"0%", "101%", "x", "0", ""} {
		if _, err := ParseDiskBudget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
