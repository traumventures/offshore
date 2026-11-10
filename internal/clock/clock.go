// Package clock abstracts the time source of scheduling decisions (idle
// stop, lifetime expiry, TTLs), so tests can move time instead of
// sleeping.
//
// Real is the default everywhere. FromEnv returns a test clock only when
// CLOUDSOURCED_TEST_CLOCK is set:
//
//	CLOUDSOURCED_TEST_CLOCK=offset:/path/to/file
//
// is real time plus the duration stored in the file (e.g. "25h"; missing
// or empty means 0). `cloudsourced debug clock advance` adds to it, so the
// processes sharing the file (daemon, control plane) see the same jump.
// Timers and tickers always run at real speed.
package clock

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Clock is a time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	Sleep(d time.Duration)
}

// Ticker delivers ticks on C until stopped.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real is the system clock.
var Real Clock = realClock{}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (realClock) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.t.C }
func (t realTicker) Stop()               { t.t.Stop() }

// Since returns the time elapsed since t on c.
func Since(c Clock, t time.Time) time.Duration { return c.Now().Sub(t) }

// EnvVar selects a test clock (see the package documentation).
const EnvVar = "CLOUDSOURCED_TEST_CLOCK"

// FromEnv returns the clock selected by CLOUDSOURCED_TEST_CLOCK, Real when
// it is unset. An invalid value is an error: a test that asks for a test
// clock must not silently run on real time.
func FromEnv() (Clock, error) {
	v := os.Getenv(EnvVar)
	if v == "" {
		return Real, nil
	}
	if path, ok := strings.CutPrefix(v, "offset:"); ok && path != "" {
		return &Offset{Path: path}, nil
	}
	return nil, fmt.Errorf("%s: unsupported value %q (want offset:FILE)", EnvVar, v)
}

// Must is FromEnv for process start up: it panics on an invalid setting.
func Must() Clock {
	c, err := FromEnv()
	if err != nil {
		panic(err)
	}
	return c
}

// OffsetFile returns the offset file of CLOUDSOURCED_TEST_CLOCK ("" if no
// offset clock is configured).
func OffsetFile() string {
	path, _ := strings.CutPrefix(os.Getenv(EnvVar), "offset:")
	if path == os.Getenv(EnvVar) {
		return ""
	}
	return path
}

// Offset is real time plus a duration read from a file. The file is read at
// most every 50ms.
type Offset struct {
	Path string

	mu     sync.Mutex
	read   time.Time
	offset time.Duration
}

// Now implements Clock.
func (o *Offset) Now() time.Time { return time.Now().Add(o.Offset()) }

// Offset returns the current offset.
func (o *Offset) Offset() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if time.Since(o.read) >= 50*time.Millisecond {
		o.offset, _ = ReadOffset(o.Path)
		o.read = time.Now()
	}
	return o.offset
}

func (o *Offset) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (o *Offset) Sleep(d time.Duration)                  { time.Sleep(d) }
func (o *Offset) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }

// ReadOffset reads an offset file (missing or empty: 0).
func ReadOffset(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// Advance adds d to the offset in path and returns the new offset.
func Advance(path string, d time.Duration) (time.Duration, error) {
	cur, err := ReadOffset(path)
	if err != nil {
		return 0, err
	}
	cur += d
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cur.String()+"\n"), 0o644); err != nil {
		return 0, err
	}
	return cur, os.Rename(tmp, path)
}
