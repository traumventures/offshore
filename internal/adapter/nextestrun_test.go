package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type recRuntime struct {
	root, cwd string
	argv      []string
}

func (r *recRuntime) Root() string        { return r.root }
func (r *recRuntime) Cwd() string         { return r.cwd }
func (r *recRuntime) BuildDirs() []string { return []string{"app/target"} }
func (r *recRuntime) Warn(string, ...any) {}
func (r *recRuntime) Remote(_ context.Context, argv []string, _ RemoteOptions) (int, error) {
	r.argv = argv
	return 1, nil
}

// The archive directory (target/offshore) does not exist in a fresh build
// directory: the remote phase creates it before nextest writes the
// archive (live: "error creating archive ... No such file or directory").
func TestNextestCreatesArchiveDir(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "cargo-nextest"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	n, ok := parseNextest(nil, []string{"run", "-p", "x"})
	if !ok {
		t.Fatal("parse")
	}
	rt := &recRuntime{root: "/w", cwd: "/w/app"}
	if code, err := runNextest(context.Background(), rt, nil, n); code != 1 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(rt.argv) < 7 || rt.argv[0] != "sh" || rt.argv[4] != "/w/app/target/offshore" || rt.argv[5] != "cargo" || rt.argv[6] != "nextest" {
		t.Fatalf("remote argv %q", rt.argv)
	}
}
