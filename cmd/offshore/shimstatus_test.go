package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectShims(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "offshore")
	shims := filepath.Join(root, "shims")
	sys := filepath.Join(root, "usr-bin")
	for _, d := range []string{shims, sys} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(sys, "cargo"), []byte("#!/bin/sh\n"), 0o755)
	if r := inspectShims(shims, sys, self); r.Active() || len(r.Tools) != 0 {
		t.Fatalf("no shims: %+v", r)
	}
	os.Symlink(self, filepath.Join(shims, "cargo"))
	os.WriteFile(filepath.Join(shims, "other"), []byte("x"), 0o755) // not a proxy

	r := inspectShims(shims, shims+":"+sys, self)
	if !r.Active() || r.Position != 1 || strings.Join(r.Tools, ",") != "cargo" {
		t.Fatalf("first: %+v", r)
	}
	r = inspectShims(shims, sys+":"+shims, self)
	if r.Active() || r.Position != 2 || r.Shadowed["cargo"] != filepath.Join(sys, "cargo") {
		t.Fatalf("shadowed: %+v", r)
	}
	r = inspectShims(shims, sys, self)
	if r.Active() || r.Position != 0 {
		t.Fatalf("absent: %+v", r)
	}
	var b strings.Builder
	r.print(&b)
	if !strings.Contains(b.String(), "not in PATH") || !strings.Contains(b.String(), "export PATH=\""+shims+":$PATH\"") {
		t.Fatal(b.String())
	}
	// Second in PATH but nothing earlier provides cargo: still active.
	r = inspectShims(shims, filepath.Join(root, "empty")+":"+shims, self)
	if !r.Active() || r.Position != 2 {
		t.Fatalf("second: %+v", r)
	}
}
