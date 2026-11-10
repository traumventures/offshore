package clock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFakeTimersAndTickers(t *testing.T) {
	t0 := time.Unix(1000, 0)
	f := NewFake(t0)
	after := f.After(10 * time.Second)
	tk := f.NewTicker(4 * time.Second)
	defer tk.Stop()
	f.Advance(5 * time.Second)
	select {
	case <-after:
		t.Fatal("timer fired early")
	default:
	}
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker did not tick")
	}
	f.Advance(5 * time.Second)
	select {
	case got := <-after:
		if !got.Equal(t0.Add(10 * time.Second)) {
			t.Fatalf("fired at %v", got)
		}
	default:
		t.Fatal("timer did not fire")
	}
	if got := Since(f, t0); got != 10*time.Second {
		t.Fatalf("Since = %v", got)
	}
	done := make(chan struct{})
	go func() { f.Sleep(time.Minute); close(done) }()
	for i := 0; i < 100; i++ { // the sleeper registers asynchronously
		f.Advance(time.Second)
		select {
		case <-done:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	<-done
}

func TestOffsetClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offset")
	t.Setenv(EnvVar, "offset:"+path)
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if d := c.Now().Sub(time.Now()); d < -time.Second || d > time.Second {
		t.Fatalf("missing offset file: skew %v", d)
	}
	if OffsetFile() != path {
		t.Fatalf("OffsetFile = %q", OffsetFile())
	}
	if _, err := Advance(path, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := Advance(path, 30*time.Minute); got != 150*time.Minute {
		t.Fatalf("offset %v", got)
	}
	time.Sleep(60 * time.Millisecond) // re-read interval
	if d := c.Now().Sub(time.Now()); d < 149*time.Minute || d > 151*time.Minute {
		t.Fatalf("offset not applied: %v", d)
	}
	t.Setenv(EnvVar, "scale:60")
	if _, err := FromEnv(); err == nil {
		t.Fatal("invalid value accepted")
	}
	t.Setenv(EnvVar, "")
	if c, _ := FromEnv(); c != Real {
		t.Fatal("real clock expected by default")
	}
}
