package orchestrator

import (
	"sort"
	"time"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// CostApproximation documents how costs are estimated.
const CostApproximation = "price_per_hour of the machine's fleet size times its observed starting/running time; " +
	"machines first seen up count from their creation time, restarts before the control plane started and " +
	"machines removed before it started are not visible, and billing granularity of the provider is ignored"

// SizeCost is the estimated cost of one machine size.
type SizeCost struct {
	Provider     string  `json:"provider"`
	Size         string  `json:"size"`
	PricePerHour float64 `json:"price_per_hour"`
	Machines     int     `json:"machines"`           // machines with uptime in the period
	Running      int     `json:"running"`            // machines up now
	UptimeSec    float64 `json:"uptime_seconds"`     // summed over machines
	Cost         float64 `json:"cost"`               // estimated, in the currency of price_per_hour
	Unpriced     bool    `json:"unpriced,omitempty"` // no price configured
}

// CostReport is the estimated fleet cost since a point in time.
type CostReport struct {
	Since         time.Time  `json:"since"`
	Sizes         []SizeCost `json:"sizes"`
	Total         float64    `json:"total"`
	PerHour       float64    `json:"per_hour"` // current burn rate of machines up now
	Approximation string     `json:"approximation"`
}

// Costs estimates the cost of machines (as returned by Snapshot, including
// removed ones) between since and now, grouped by size.
func Costs(ms []Machine, since, now time.Time) CostReport {
	r := CostReport{Since: since, Approximation: CostApproximation}
	idx := map[[2]string]int{}
	for i := range ms {
		m := &ms[i]
		up := m.UpTime(since, now)
		running := m.Removed.IsZero() && (m.State == provider.Running || m.State == provider.Starting)
		if up <= 0 && !running {
			continue
		}
		size := m.Labels[provider.LabelSize]
		k := [2]string{m.Provider, size}
		j, ok := idx[k]
		if !ok {
			j = len(r.Sizes)
			idx[k] = j
			r.Sizes = append(r.Sizes, SizeCost{Provider: m.Provider, Size: size, PricePerHour: m.PricePerHour, Unpriced: m.PricePerHour == 0})
		}
		sc := &r.Sizes[j]
		sc.Machines++
		sc.UptimeSec += up.Seconds()
		c := m.PricePerHour * up.Hours()
		sc.Cost += c
		r.Total += c
		if running {
			sc.Running++
			r.PerHour += m.PricePerHour
		}
	}
	sort.Slice(r.Sizes, func(i, j int) bool {
		if r.Sizes[i].Provider != r.Sizes[j].Provider {
			return r.Sizes[i].Provider < r.Sizes[j].Provider
		}
		return r.Sizes[i].Size < r.Sizes[j].Size
	})
	return r
}

// StartOfDay returns local midnight of t.
func StartOfDay(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}
