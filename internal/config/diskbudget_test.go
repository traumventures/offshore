package config

import (
	"strings"
	"testing"
)

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

func TestParseSourceCache(t *testing.T) {
	b, err := ParseSourceCache(DefaultSourceCache)
	if err != nil || b.Of(800) != 100 {
		t.Fatalf("default: %+v %v", b, err)
	}
	if b, err := ParseSourceCache("20G"); err != nil || b.Of(100<<30) != 20<<30 {
		t.Fatalf("20G: %+v %v", b, err)
	}
	if _, err := ParseSourceCache("0%"); err == nil || !strings.Contains(err.Error(), "source cache") {
		t.Fatalf("0%%: %v", err)
	}
}
