package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/top"
)

// topCmd runs `offshore top`.
func topCmd(args []string) error {
	fs := newFlagSet("top")
	once := fs.Bool("once", false, "print one plain text frame and exit (for agents and scripts)")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval")
	width := fs.Int("width", 0, "frame width for --once (default: terminal width or 140)")
	window := fs.Duration("window", 10*time.Minute, "history window of the graphs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	if *once {
		s, err := fetchTop(ctx)
		if err != nil {
			return err
		}
		h := top.NewHistory(0)
		if evs, err := seedTop(ctx, s.Time.Add(-*window)); err == nil {
			h.Seed(evs)
		}
		h.Add(s)
		v := top.DefaultView()
		v.Window = *window
		v.Width = *width
		if v.Width <= 0 {
			v.Width = 140
			if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
				v.Width = w
			}
		}
		var r top.Rates
		fmt.Println(top.Frame(s, h, v, r.Update(s), top.PlainStyles()))
		return nil
	}
	if *interval < 500*time.Millisecond {
		*interval = 500 * time.Millisecond
	}
	var since time.Time
	fetch := func(ctx context.Context) (top.Snapshot, error) {
		s, err := fetchTop(ctx)
		if err != nil {
			return s, err
		}
		// Events since the previous poll: finished sessions, placements,
		// machine lifecycle (the first poll leaves them to the seed).
		if !since.IsZero() {
			if evs, err := seedTop(ctx, since.Add(-5*time.Second)); err == nil { // overlap: History dedupes
				s.Events = evs
			}
		}
		since = s.Time
		return s, nil
	}
	return top.Run(fetch, seedTop, *interval)
}

// fetchTop polls machines, queue and today's cost through the daemon.
func fetchTop(ctx context.Context) (top.Snapshot, error) {
	raw, err := daemon.Call(ctx, daemon.Request{Op: "status"}, nil)
	if err != nil {
		return top.Snapshot{}, err
	}
	s := top.Snapshot{Time: time.Now()}
	if err := json.Unmarshal(raw, &s.Machines); err != nil {
		return s, err
	}
	if raw, err := daemon.Call(ctx, daemon.Request{Op: "queue"}, nil); err == nil {
		s.QueueKnown = json.Unmarshal(raw, &s.Queue) == nil
	}
	s.Cost = orchestrator.Costs(s.Machines, orchestrator.StartOfDay(s.Time), s.Time)
	return s, nil
}

// seedTop reads machine samples since a time for the graphs.
func seedTop(ctx context.Context, since time.Time) ([]events.Event, error) {
	raw, err := daemon.Call(ctx, daemon.Request{Op: "events", Since: since}, nil)
	if err != nil {
		return nil, err
	}
	var evs []events.Event
	err = json.Unmarshal(raw, &evs)
	return evs, err
}
