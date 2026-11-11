package pool

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/image"
)

// Prebuild pushes to every remote registry right away (EnsureEnv builds
// only for local image stores and leaves pushes to machine creation), and
// EnvState reports the result without building anything.
func TestPrebuildPushesEverywhere(t *testing.T) {
	if image.Native() != image.LinuxAMD64 {
		t.Skip("the fake providers run linux/amd64")
	}
	b := &countingBuilder{}
	east, west := &pushRegistry{repo: "ecr/east"}, &pushRegistry{repo: "ecr/west"}
	l := multiPool(t, map[string]*hostProvider{
		"local": {name: "local-docker", registry: localStore{b}, builder: b},
		"east":  {name: "aws-ec2", registry: east, builder: b},
		"west":  {name: "aws-ec2", registry: west, builder: b},
	})
	env := testEnv(t)
	ctx := context.Background()
	st, err := l.EnvState(ctx, env.Hash, env.Tag)
	if err != nil || st.Known || len(st.Ready) != 0 || len(st.Missing) != 3 || len(b.builds) != 0 {
		t.Fatalf("before: %+v %v builds %v", st, err, b.builds)
	}
	if err := l.Prebuild(ctx, env, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(b.builds) != 1 || len(east.pushes) != 1 || len(west.pushes) != 1 {
		t.Fatalf("builds %v pushes %v %v", b.builds, east.pushes, west.pushes)
	}
	st, _ = l.EnvState(ctx, env.Hash, env.Tag)
	if !st.Known || len(st.Ready) != 3 || len(st.Missing) != 0 || st.Building {
		t.Fatalf("after: %+v", st)
	}
	// A second prebuild finds everything ready.
	if err := l.Prebuild(ctx, env, io.Discard); err != nil || len(b.builds) != 1 || len(east.pushes) != 1 {
		t.Fatalf("rebuilt: %v %v %v", err, b.builds, east.pushes)
	}
}

// The control plane runs a requested prebuild detached and answers state
// queries without building.
func TestServerPrebuildAndState(t *testing.T) {
	if image.Native() != image.LinuxAMD64 {
		t.Skip("the fake providers run linux/amd64")
	}
	b := &countingBuilder{}
	reg := &pushRegistry{repo: "ecr/r"}
	l := multiPool(t, map[string]*hostProvider{"aws": {name: "aws-ec2", registry: reg, builder: b}})
	srv, err := NewServer(l, "k", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	r, err := NewRemote(hs.URL, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnv(t)
	ctx := context.Background()
	st, err := r.EnvState(ctx, env.Hash, env.Tag)
	if err != nil || st.Known || len(st.Missing) != 1 {
		t.Fatalf("state %+v %v", st, err)
	}
	if err := r.Prebuild(ctx, env, io.Discard); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		reg.mu.Lock()
		n := len(reg.pushes)
		reg.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no push after prebuild")
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, err = r.EnvState(ctx, env.Hash, env.Tag)
	if err != nil || !st.Known || len(st.Ready) != 1 {
		t.Fatalf("state %+v %v", st, err)
	}
	if ts, err := r.Health(ctx); err != nil || time.Since(ts) > time.Minute {
		t.Fatalf("health %v %v", ts, err)
	}
}
