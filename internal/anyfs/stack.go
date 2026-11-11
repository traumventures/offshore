package anyfs

import (
	"fmt"
	"time"
)

// DefaultPolicy returns the link policy between two media: a local disk
// over a volume is an exclusive write-back pair (the volume is the
// victim cache of the local disk), everything over the store or a peer
// is an inclusive write-back cache.
func DefaultPolicy(upper, lower string) Policy {
	p := Policy{Inclusion: Inclusive, Write: WriteBack, DirtyRatio: 0.1, DirtyExpire: 30 * time.Second}
	if upper == "local" && lower == "volume" {
		p.Inclusion = Exclusive
	}
	return p
}

// Fold stacks tiers top first into one tier: Fold(local, volume, store)
// is Link(local, Link(volume, store)). pols[i] is the policy of the link
// below tiers[i] (missing: DefaultPolicy of the media).
func Fold(obs Observer, pols []Policy, tiers ...Tier) (Tier, error) {
	if len(tiers) == 0 {
		return nil, fmt.Errorf("anyfs: empty stack")
	}
	t := tiers[len(tiers)-1]
	for i := len(tiers) - 2; i >= 0; i-- {
		var p Policy
		if i < len(pols) {
			p = pols[i]
		} else {
			p = DefaultPolicy(tiers[i].Profile().Top().Medium, t.Profile().Top().Medium)
		}
		t = NewLink(tiers[i], t, p, obs)
	}
	return t, nil
}

// Links returns the links of a folded stack, top first.
func Links(t Tier) []*Link {
	var out []*Link
	for {
		l, ok := t.(*Link)
		if !ok {
			return out
		}
		out = append(out, l)
		t = l.lower
	}
}

// StartAll starts the writeback of every link of a stack.
func StartAll(t Tier) {
	for _, l := range Links(t) {
		l.Start()
	}
}

// CloseAll stops the writeback of every link of a stack.
func CloseAll(t Tier) {
	for _, l := range Links(t) {
		l.Close()
	}
}
