package scheduler

import "time"

// WarmWindow keeps Machines running per recently used environment on the
// given weekdays between From and To (offsets since local midnight). It
// raises the warm pool above Config.MinMachines during working hours.
type WarmWindow struct {
	Days     []time.Weekday // empty: every day
	From, To time.Duration  // From > To spans midnight (e.g. 22:00-06:00)
	Machines int
	Location *time.Location // nil: UTC
}

// Active reports whether the window covers t.
func (w WarmWindow) Active(t time.Time) bool {
	loc := w.Location
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	off := t.Sub(midnight)
	day := t.Weekday()
	if w.From > w.To { // spans midnight: the part after midnight belongs to the previous day
		if off < w.To {
			day = (day + 6) % 7
		} else if off < w.From {
			return false
		}
	} else if off < w.From || off >= w.To {
		return false
	}
	if len(w.Days) == 0 {
		return true
	}
	for _, d := range w.Days {
		if d == day {
			return true
		}
	}
	return false
}

// scheduledWarm returns the machines the warm windows ask for at t.
func scheduledWarm(ws []WarmWindow, t time.Time) int {
	n := 0
	for _, w := range ws {
		if w.Active(t) && w.Machines > n {
			n = w.Machines
		}
	}
	return n
}
