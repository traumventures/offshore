//go:build awssmoke

// Live boot measurement of the aws-ec2 provider (real account, real
// cost); never part of the regular test run:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/cs-aws-smoke/offshore-linux-amd64 ./cmd/offshore
//	go test -c -tags awssmoke -o /tmp/cs-aws-smoke/smoke.test ./internal/orchestrator
//	SMOKE_IMAGE=<ecr ref> SMOKE_ENV=<env hash> AWS_PROFILE=... /tmp/cs-aws-smoke/smoke.test -test.run TestAWSBoot -test.v
//
// TestAWSBoot creates one machine through the orchestrator (owner
// "boot-smoke", lifetime 40 minutes as a safety net), prints its boot
// phases, stops and starts it (the reuse path) and destroys it.

package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/provider"
	_ "github.com/traumventures/offshore/internal/provider/awsec2"
	"github.com/traumventures/offshore/internal/scheduler"
)

func TestAWSBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	img, env := os.Getenv("SMOKE_IMAGE"), os.Getenv("SMOKE_ENV")
	if img == "" || env == "" {
		t.Fatal("SMOKE_IMAGE and SMOKE_ENV are required")
	}
	opts := map[string]any{
		"region":       "eu-central-1",
		"disk_gb":      int64(200),
		"binary_amd64": "/tmp/cs-aws-smoke/offshore-linux-amd64",
		"allow_cidrs":  []any{os.Getenv("SMOKE_CIDR")},
	}
	if p := os.Getenv("SMOKE_PROFILE"); p != "" {
		opts["profile"] = p
	}
	p, err := provider.Open("aws-ec2", opts)
	if err != nil {
		t.Fatal(err)
	}
	types := strings.Split(envOr("SMOKE_TYPES", "m8a.2xlarge,m7a.2xlarge,m6a.2xlarge"), ",")
	size := scheduler.Size{Provider: "aws", Name: "x8", Arch: "amd64", Capacity: scheduler.Resources{CPU: 8, Memory: 30 << 30, Disk: 200 << 30},
		InstanceTypes: types, PricePerHour: 0.58}
	cfg := Config{Owner: "boot-smoke", MaxLifetime: 40 * time.Minute, Sizes: []scheduler.Size{size}}
	o, err := New(cfg, map[string]provider.Provider{"aws": p}, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	var ready []events.Event
	o.obs.log.Tap(func(ev events.Event) {
		if ev.Type == events.MachineReady {
			ready = append(ready, ev)
		}
	})
	id := "smoke" + time.Now().UTC().Format("0102150405")
	defer func() {
		dctx, dc := context.WithTimeout(context.Background(), 5*time.Minute)
		defer dc()
		if err := p.Destroy(dctx, id); err != nil {
			t.Errorf("destroy: %v", err)
		}
		ms, err := p.List(dctx, cfg.Owner)
		t.Logf("after destroy: %d machines listed (%v)", len(ms), err)
		for _, m := range ms {
			t.Logf("  %s %s", m.ID, m.State)
		}
	}()
	report := func(what string) {
		if len(ready) == 0 {
			t.Errorf("%s: no machine_ready event", what)
			return
		}
		ev := ready[len(ready)-1]
		t.Logf("%s: ready after %d ms", what, ev.LatencyMs)
		prev := int64(0)
		for _, ph := range ev.Boot {
			t.Logf("  %-15s %7d ms  (+%6d ms)  %d bytes", ph.Phase, ph.Ms, ph.Ms-prev, ph.Bytes)
			prev = ph.Ms
		}
	}
	o.mu.Lock()
	o.creating[id] = creation{prov: "aws", env: env, size: size, at: o.now()}
	o.mu.Unlock()
	if err := o.CreateMachine(ctx, id, env, img, size); err != nil {
		t.Fatalf("create: %v", err)
	}
	report("create")
	if os.Getenv("SMOKE_RESTART") == "" {
		return
	}
	if err := p.Stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for {
		ms, err := p.List(ctx, cfg.Owner)
		if err == nil && len(ms) == 1 && ms[0].State == provider.Stopped {
			break
		}
		time.Sleep(2 * time.Second)
	}
	o.mu.Lock()
	o.creating[id] = creation{prov: "aws", env: env, size: size, at: o.now(), started: true}
	o.mu.Unlock()
	o.startMachine(ctx, id)
	report("start")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
