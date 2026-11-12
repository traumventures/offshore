package scheduler

import (
	"testing"
	"time"
)

var aws = []Size{
	{Provider: "aws", Name: "large", Capacity: res(2, 4), PricePerHour: 0.1},
	{Provider: "aws", Name: "xlarge", Capacity: res(4, 8), PricePerHour: 0.2},
	{Provider: "aws", Name: "4xlarge", Capacity: res(16, 32), PricePerHour: 0.8},
}

func busy(id string, cap, reserved Resources, sessions int) Machine {
	m := machine(id, cap, reserved)
	m.Sessions = sessions
	return m
}

func TestPlaceBestFit(t *testing.T) {
	strict := Config{CPUOvercommit: 1, MemoryOvercommit: 1}
	warm := busy("warm", res(8, 16), res(6, 12), 3)
	warm.WarmRepos = []string{"r"}
	tests := []struct {
		name     string
		cfg      Config
		machines []Machine
		task     Task
		want     string
		reason   string
	}{
		{"tightest fit wins", strict, []Machine{busy("empty", res(8, 16), Resources{}, 0), busy("half", res(8, 16), res(4, 8), 2), busy("full", res(8, 16), res(7, 15), 4)}, task(2, 4), "half", "best fit"},
		{"smaller machine is the tighter fit", strict, []Machine{machine("big", res(16, 32), Resources{}), machine("small", res(4, 8), Resources{})}, task(2, 4), "small", "best fit"},
		{"warm cache beats fit", strict, []Machine{busy("half", res(8, 16), res(4, 8), 2), warm}, task(2, 4), "warm", "warm cache"},
		{"physical room beats overcommit", Config{CPUOvercommit: 2, MemoryOvercommit: 1}, []Machine{busy("over", res(4, 16), res(4, 8), 2), busy("room", res(8, 16), res(2, 4), 1)}, task(2, 4), "room", "best fit"},
		{"single candidate", strict, []Machine{busy("full", res(8, 16), res(8, 16), 4), busy("only", res(8, 16), res(2, 2), 1)}, task(2, 4), "only", "rendezvous"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pl := Default{Cfg: tc.cfg}.Place(Pool{Now: now, Machines: tc.machines}, tc.task)
			if pl.Kind != Assign || pl.Machine != tc.want {
				t.Fatalf("got %+v, want %s (%s)", pl, tc.want, tc.reason)
			}
		})
	}
}

func queue(tasks ...Task) []Task {
	for i := range tasks {
		tasks[i].ID = string(rune('a' + i))
		tasks[i].Since = now.Add(time.Duration(i) * time.Second)
	}
	return tasks
}

func creates(acts []Action) []string {
	var out []string
	for _, a := range acts {
		if a.Kind == Create {
			out = append(out, a.Size.Name)
		}
	}
	return out
}

func kinds(acts []Action) map[ActionKind][]string {
	out := map[ActionKind][]string{}
	for _, a := range acts {
		out[a.Kind] = append(out[a.Kind], a.Machine)
	}
	return out
}

func TestWarmWindowActive(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}
	weekdays := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	day := WarmWindow{Days: weekdays, From: 8 * time.Hour, To: 19 * time.Hour, Machines: 1, Location: berlin}
	night := WarmWindow{Days: []time.Weekday{time.Friday}, From: 22 * time.Hour, To: 6 * time.Hour, Machines: 2}
	mon := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) // Monday
	tests := []struct {
		name string
		w    WarmWindow
		at   time.Time
		want bool
	}{
		{"monday 09:00 Berlin", day, time.Date(2026, 11, 9, 9, 0, 0, 0, berlin), true},
		{"monday 07:30 UTC is 08:30 Berlin", day, mon.Add(7*time.Hour + 30*time.Minute), true},
		{"monday 18:30 UTC is 19:30 Berlin", day, mon.Add(18*time.Hour + 30*time.Minute), false},
		{"to is exclusive", day, time.Date(2026, 11, 9, 19, 0, 0, 0, berlin), false},
		{"saturday", day, time.Date(2026, 11, 14, 10, 0, 0, 0, berlin), false},
		{"friday 23:00 overnight", night, mon.Add(4*24*time.Hour + 23*time.Hour), true},
		{"saturday 05:00 belongs to friday", night, mon.Add(5*24*time.Hour + 5*time.Hour), true},
		{"friday 05:00 belongs to thursday", night, mon.Add(4*24*time.Hour + 5*time.Hour), false},
		{"saturday 07:00", night, mon.Add(5*24*time.Hour + 7*time.Hour), false},
		{"every day", WarmWindow{From: 0, To: 24 * time.Hour}, mon.Add(6*24*time.Hour + 12*time.Hour), true},
	}
	for _, tc := range tests {
		if got := tc.w.Active(tc.at); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestPlanScheduledWarmPool(t *testing.T) {
	mon9 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	cfg := Config{IdleStop: time.Minute,
		WarmPool: []WarmWindow{{From: 8 * time.Hour, To: 19 * time.Hour, Machines: 2, Days: []time.Weekday{time.Monday}}}}
	st := machine("st", res(4, 8), Resources{})
	st.State = Stopped
	st.ExpiresAt = mon9.Add(time.Hour)
	recent := map[string]time.Time{"e": mon9.Add(-48 * time.Hour)} // older than a day: min_machines would not apply
	acts := Default{Cfg: cfg}.Plan(Pool{Now: mon9, Sizes: aws, Machines: []Machine{st}, Recent: recent})
	got := kinds(acts)
	if len(got[Start]) != 1 || len(got[Create]) != 1 {
		t.Fatalf("want start + create for a pool of 2, got %+v", acts)
	}
	// Outside the window idle machines stop.
	idle := machine("i", res(4, 8), Resources{})
	idle.IdleSince = mon9.Add(-time.Hour)
	evening := mon9.Add(11 * time.Hour)
	idle.ExpiresAt = evening.Add(time.Hour)
	got = kinds(Default{Cfg: cfg}.Plan(Pool{Now: evening, Sizes: aws, Machines: []Machine{idle}, Recent: recent}))
	if len(got[Stop]) != 1 {
		t.Fatalf("want stop after hours, got %v", got)
	}
	// Inside the window the idle machine is kept.
	got = kinds(Default{Cfg: cfg}.Plan(Pool{Now: mon9, Sizes: aws, Machines: []Machine{idle}, Recent: recent}))
	if len(got[Stop]) != 0 || len(got[Create]) != 1 {
		t.Fatalf("want idle machine kept and one more created, got %v", got)
	}
}
