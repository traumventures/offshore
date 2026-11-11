package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/provider"
)

func TestAlreadyWarm(t *testing.T) {
	now := time.Now()
	warm := orchestrator.Machine{Machine: provider.Machine{ID: "m1", State: provider.Running, EnvHash: "e1"},
		Status: &proto.Status{Ready: true, WarmRepos: []string{"r1"}}}
	ms := []orchestrator.Machine{warm}
	if _, ok := alreadyWarm(ms, "e1", "r1", nil, nil, "", now); !ok {
		t.Fatal("warm slot without warm-up command must skip")
	}
	for name, c := range map[string]struct {
		ms        []orchestrator.Machine
		env, repo string
	}{
		"other env":  {ms, "e2", "r1"},
		"other repo": {ms, "e1", "r2"},
		"stopped":    {[]orchestrator.Machine{{Machine: provider.Machine{ID: "m1", State: provider.Stopped, EnvHash: "e1"}, Status: warm.Status}}, "e1", "r1"},
		"draining":   {[]orchestrator.Machine{{Machine: warm.Machine, Status: &proto.Status{Ready: true, Draining: true, WarmRepos: []string{"r1"}}}}, "e1", "r1"},
		"no status":  {[]orchestrator.Machine{{Machine: warm.Machine}}, "e1", "r1"},
	} {
		if _, ok := alreadyWarm(c.ms, c.env, c.repo, nil, nil, "", now); ok {
			t.Errorf("%s: must not count as warm", name)
		}
	}
	run := []string{"cargo", "check", "--workspace"}
	st := &prewarmStamp{Time: now.Add(-time.Hour), Head: "abc", Env: "e1", RepoID: "r1", Run: run}
	if _, ok := alreadyWarm(ms, "e1", "r1", run, st, "abc", now); !ok {
		t.Fatal("same commit, recent warm-up: skip")
	}
	if _, ok := alreadyWarm(ms, "e1", "r1", run, st, "def", now); ok {
		t.Fatal("new commit must warm up again")
	}
	if _, ok := alreadyWarm(ms, "e1", "r1", run, st, "abc", now.Add(prewarmFresh)); ok {
		t.Fatal("stale warm-up must run again")
	}
	if _, ok := alreadyWarm(ms, "e1", "r1", []string{"cargo", "build"}, st, "abc", now); ok {
		t.Fatal("changed warm-up command must run")
	}
	if _, ok := alreadyWarm(ms, "e1", "r1", run, nil, "abc", now); ok {
		t.Fatal("no stamp must run")
	}
}

func TestWhyShowsLabels(t *testing.T) {
	exit := 0
	r := events.Run{Time: time.Now(), Tool: "cargo", Args: []string{"check"}, Mode: events.Remote, Exit: &exit,
		Labels: map[string]string{"agent": "codex", "worktree": "/w/a", "purpose": "prewarm"}}
	var b bytes.Buffer
	printRun(&b, r)
	if !strings.Contains(b.String(), "labels     worktree=/w/a agent=codex purpose=prewarm") {
		t.Fatalf("why:\n%s", b.String())
	}
	b.Reset()
	printRunList(&b, []events.Run{r})
	if !strings.Contains(b.String(), "remote (prewarm)") || !strings.Contains(b.String(), "codex") {
		t.Fatalf("why list:\n%s", b.String())
	}
	se := proto.Session{ID: "s1", Argv: []string{"cargo", "build"}, Labels: map[string]string{"user": "u", "host": "box", "agent": "codex", "worktree": "/w"}}
	if got := sessionLine("m1", se, 90*time.Second); got != "  m1 session s1: u@box cargo build (worktree=/w agent=codex, running 1m30s)" {
		t.Fatalf("status line %q", got)
	}
	r.Labels["user"], r.Labels["host"] = "u", "box"
	b.Reset()
	printRun(&b, r)
	if !strings.Contains(b.String(), "client     u@box") {
		t.Fatalf("why lacks the client:\n%s", b.String())
	}
}
