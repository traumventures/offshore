package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/traumventures/cloudsourced/internal/proto"
)

func TestRelocateToolchain(t *testing.T) {
	img := t.TempDir()
	cargo, rustup := filepath.Join(img, "cargo"), filepath.Join(img, "rustup")
	os.MkdirAll(cargo, 0o755)
	os.MkdirAll(rustup, 0o755)
	env := map[string]string{"CARGO_HOME": cargo, "RUSTUP_HOME": rustup, "PATH": cargo + "/bin:/usr/bin"}
	all := func(string) bool { return true }
	tc := &proto.Toolchain{CargoHome: "/home/u/.cargo", RustupHome: "/home/u/.rustup"}

	r := relocateToolchain(env, tc, []string{"/home/u/src/repo"}, all)
	if fmt.Sprint(r.binds) != fmt.Sprintf("[{%s /home/u/.cargo false} {%s /home/u/.rustup false}]", cargo, rustup) {
		t.Fatalf("binds = %v", r.binds)
	}
	if fmt.Sprint(r.env) != "[[CARGO_HOME /home/u/.cargo] [PATH /home/u/.cargo/bin:/usr/bin] [RUSTUP_HOME /home/u/.rustup]]" {
		t.Fatalf("env = %v", r.env)
	}
	if r := relocateToolchain(env, nil, nil, all); len(r.binds)+len(r.env) != 0 {
		t.Fatal("no toolchain requested")
	}
	// Never over or into projected sources, never over occupied paths.
	r = relocateToolchain(env, &proto.Toolchain{CargoHome: "/home/u", RustupHome: "/home/u/src/repo/x"}, []string{"/home/u/src/repo"}, all)
	if len(r.binds) != 0 || len(r.env) != 0 {
		t.Fatalf("unsafe binds: %v %v", r.binds, r.env)
	}
	r = relocateToolchain(env, tc, nil, func(string) bool { return false })
	if len(r.binds) != 0 {
		t.Fatalf("occupied: %v", r.binds)
	}
	// Same path as in the image: environment only.
	r = relocateToolchain(env, &proto.Toolchain{CargoHome: cargo, RustupHome: rustup}, nil, all)
	if len(r.binds) != 0 || len(r.env) != 3 {
		t.Fatalf("same path: %v %v", r.binds, r.env)
	}
	// No toolchain in the image.
	if r := relocateToolchain(map[string]string{"HOME": img}, tc, nil, all); len(r.binds) != 0 {
		t.Fatalf("missing image toolchain: %v", r.binds)
	}
}

func TestDirUsable(t *testing.T) {
	d := t.TempDir()
	if !dirUsable(filepath.Join(d, "missing")) || !dirUsable(d) {
		t.Fatal("missing and empty directories are usable")
	}
	os.WriteFile(filepath.Join(d, "f"), nil, 0o644)
	if dirUsable(d) || dirUsable(filepath.Join(d, "f")) {
		t.Fatal("occupied paths are not usable")
	}
}
