package orchestrator

import (
	"math"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/provider"
)

func TestUptimeObservation(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	m := &Machine{Machine: provider.Machine{ID: "a", State: provider.Running, CreatedAt: t0.Add(-time.Hour)}}
	m.observe(t0) // first seen up: counts from creation
	m.State = provider.Stopped
	m.observe(t0.Add(30 * time.Minute))
	m.observe(t0.Add(40 * time.Minute)) // still stopped
	m.State = provider.Starting
	m.observe(t0.Add(2 * time.Hour))
	m.State = provider.Running
	m.observe(t0.Add(2*time.Hour + time.Minute))
	if len(m.Uptime) != 2 {
		t.Fatalf("spans %+v", m.Uptime)
	}
	now := t0.Add(3 * time.Hour)
	if got := m.UpTime(time.Time{}, now); got != 90*time.Minute+time.Hour {
		t.Fatalf("uptime %s", got)
	}
	// Only the part after "since" counts.
	if got := m.UpTime(t0, now); got != 30*time.Minute+time.Hour {
		t.Fatalf("uptime since t0 %s", got)
	}
	// Old closed spans are pruned.
	m.State = provider.Stopped
	m.observe(now)
	m.observe(now.Add(3 * historyKeep))
	if len(m.Uptime) != 0 {
		t.Fatalf("history not pruned: %+v", m.Uptime)
	}
}

func TestCosts(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.Local)
	now := day.Add(10 * time.Hour)
	lbl := func(s string) map[string]string { return map[string]string{provider.LabelSize: s} }
	ms := []Machine{
		// Up since yesterday: only today counts (10h).
		{Machine: provider.Machine{ID: "a", State: provider.Running, Labels: lbl("large")}, Provider: "aws", PricePerHour: 1.0,
			Uptime: []Span{{Start: day.Add(-5 * time.Hour)}}},
		// Ran 2h this morning, now stopped.
		{Machine: provider.Machine{ID: "b", State: provider.Stopped, Labels: lbl("large")}, Provider: "aws", PricePerHour: 1.0,
			Uptime: []Span{{Start: day.Add(time.Hour), End: day.Add(3 * time.Hour)}}},
		// Removed after 30 minutes.
		{Machine: provider.Machine{ID: "c", State: provider.Failed, Labels: lbl("small")}, Provider: "aws", PricePerHour: 0.2,
			Uptime: []Span{{Start: day.Add(4 * time.Hour), End: day.Add(4*time.Hour + 30*time.Minute)}}, Removed: day.Add(5 * time.Hour)},
		// Local machine without price.
		{Machine: provider.Machine{ID: "d", State: provider.Starting, Labels: lbl("default")}, Provider: "docker",
			Uptime: []Span{{Start: now.Add(-time.Minute)}}},
		// Stopped all day: not listed.
		{Machine: provider.Machine{ID: "e", State: provider.Stopped, Labels: lbl("small")}, Provider: "aws", PricePerHour: 0.2},
	}
	r := Costs(ms, day, now)
	if len(r.Sizes) != 3 {
		t.Fatalf("sizes %+v", r.Sizes)
	}
	large, small, docker := r.Sizes[0], r.Sizes[1], r.Sizes[2]
	if large.Size != "large" || large.Machines != 2 || large.Running != 1 || math.Abs(large.Cost-12) > 1e-9 {
		t.Fatalf("large %+v", large)
	}
	if small.Size != "small" || small.Machines != 1 || math.Abs(small.Cost-0.1) > 1e-9 {
		t.Fatalf("small %+v", small)
	}
	if docker.Provider != "docker" || !docker.Unpriced || docker.Cost != 0 || docker.Running != 1 {
		t.Fatalf("docker %+v", docker)
	}
	if math.Abs(r.Total-12.1) > 1e-9 || r.PerHour != 1.0 {
		t.Fatalf("total %v per hour %v", r.Total, r.PerHour)
	}
	if !StartOfDay(now).Equal(day) {
		t.Fatal("start of day")
	}
}
