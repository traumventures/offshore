package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseIngress(t *testing.T) {
	if _, err := ParseIngress("aws-ec2", map[string]any{OptSharedControlPlane: true}); err == nil || !strings.Contains(err.Error(), "required with a shared control plane") {
		t.Fatalf("shared without allow_cidrs: %v", err)
	}
	in, err := ParseIngress("aws-ec2", map[string]any{})
	if err != nil || in.Explicit() {
		t.Fatalf("embedded default: %+v %v", in, err)
	}
	in, err = ParseIngress("oci", map[string]any{OptSharedControlPlane: true, "allow_cidrs": []any{"10.0.0.0/8"}})
	if err != nil || !in.Explicit() || in.CIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("explicit: %+v %v", in, err)
	}
	if _, err := ParseIngress("oci", map[string]any{"allow_cidrs": []any{"1.2.3.4"}}); err == nil {
		t.Fatal("bare IP accepted")
	}
}

func TestDiffCIDRs(t *testing.T) {
	add, rm := DiffCIDRs([]string{"1.1.1.1/32", "2.2.2.2/32"}, []string{"2.2.2.2/32", "3.3.3.3/32"}, true)
	if !reflect.DeepEqual(add, []string{"3.3.3.3/32"}) || !reflect.DeepEqual(rm, []string{"1.1.1.1/32"}) {
		t.Fatalf("authoritative: %v %v", add, rm)
	}
	add, rm = DiffCIDRs([]string{"1.1.1.1/32"}, []string{"3.3.3.3/32"}, false)
	if !reflect.DeepEqual(add, []string{"3.3.3.3/32"}) || rm != nil {
		t.Fatalf("additive: %v %v", add, rm)
	}
}
