package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a manually advanced clock for tests. Timers and tickers fire when
// Advance moves the time past their deadline; Sleep blocks until then.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	at      time.Time
	every   time.Duration // >0: ticker
	c       chan time.Time
	stopped bool
}

// NewFake returns a fake clock at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the clock to t (also backwards; nothing fires then).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
	f.fire()
}

// Advance moves the clock forward by d and fires due timers and tickers.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	f.fire()
}

func (f *Fake) fire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].at.Before(f.waiters[j].at) })
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if w.stopped {
			continue
		}
		for !w.at.After(f.now) {
			select {
			case w.c <- f.now:
			default: // like time.Ticker: drop ticks nobody reads
			}
			if w.every <= 0 {
				w.stopped = true
				break
			}
			w.at = w.at.Add(w.every)
		}
		if !w.stopped {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
}

func (f *Fake) add(d, every time.Duration) *waiter {
	f.mu.Lock()
	w := &waiter{at: f.now.Add(d), every: every, c: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	f.mu.Unlock()
	if d <= 0 {
		f.fire()
	}
	return w
}

// After implements Clock.
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.add(d, 0).c }

// Sleep implements Clock: it returns once the clock was advanced by d.
func (f *Fake) Sleep(d time.Duration) { <-f.After(d) }

// NewTicker implements Clock.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive ticker interval")
	}
	return &fakeTicker{f: f, w: f.add(d, d)}
}

type fakeTicker struct {
	f *Fake
	w *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.c }
func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	t.w.stopped = true
	t.f.mu.Unlock()
}
