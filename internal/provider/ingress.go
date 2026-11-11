package provider

import (
	"fmt"
	"net"
	"slices"
)

// OptSharedControlPlane is the provider option the pool sets (true) when the
// orchestrator runs as a shared control plane. Cloud providers then require
// an explicit allow_cidrs: the orchestrator's own address is not where the
// clients connect from.
const OptSharedControlPlane = "shared_control_plane"

// Ingress is the QUIC ingress policy of a cloud provider (UDP 7443).
type Ingress struct {
	// CIDRs is the explicit allow_cidrs list. When set it is authoritative:
	// QUIC rules for other sources are removed.
	CIDRs []string
}

// Explicit reports whether allow_cidrs was configured. Without it (only
// possible in embedded mode) the orchestrator's own public address is added
// and existing rules are kept, so several embedded orchestrators can share
// one stack.
func (i Ingress) Explicit() bool { return len(i.CIDRs) > 0 }

// ParseIngress reads allow_cidrs from provider options. With a shared
// control plane allow_cidrs is required.
func ParseIngress(kind string, opts map[string]any) (Ingress, error) {
	cidrs := StringList(opts["allow_cidrs"])
	for _, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return Ingress{}, fmt.Errorf("%s: allow_cidrs: %q is not a CIDR", kind, c)
		}
	}
	if shared, _ := opts[OptSharedControlPlane].(bool); shared && len(cidrs) == 0 {
		return Ingress{}, fmt.Errorf("%s: allow_cidrs is required with a shared control plane: list the CIDRs the clients connect from (UDP 7443), or [\"0.0.0.0/0\"] to admit any client (sessions still need a lease token)", kind)
	}
	return Ingress{CIDRs: slices.Clone(cidrs)}, nil
}

// DiffCIDRs returns the sources to add and, when authoritative, the current
// sources to remove so that the rules match desired.
func DiffCIDRs(current, desired []string, authoritative bool) (add, remove []string) {
	for _, d := range desired {
		if !slices.Contains(current, d) && !slices.Contains(add, d) {
			add = append(add, d)
		}
	}
	if authoritative {
		for _, c := range current {
			if !slices.Contains(desired, c) && !slices.Contains(remove, c) {
				remove = append(remove, c)
			}
		}
	}
	return add, remove
}
