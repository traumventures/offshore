package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePorts(t *testing.T) {
	got, err := ParsePorts([]any{int64(8080), "3000:3001", " 9000 "})
	if err != nil {
		t.Fatal(err)
	}
	want := []Port{{8080, 8080}, {3000, 3001}, {9000, 9000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got[1].String() != "3000:3001" || got[0].String() != "8080" {
		t.Fatalf("String: %s %s", got[0], got[1])
	}
	env, err := ParsePortsEnv("8080, 3000:3001,")
	if err != nil || !reflect.DeepEqual(env, want[:2]) {
		t.Fatalf("env: %v %v", env, err)
	}
	for _, bad := range [][]any{{int64(0)}, {"70000"}, {"a:1"}, {"1:b"}, {true}, {1.5}, {int64(80), "80:81"}} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestRepoPortsConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := "version = 1\n[environment]\nimage = \"x\"\n[tool.npm]\nports = [8080, \"3000:3001\"]\n"
	os.WriteFile(filepath.Join(dir, RepoFileName), []byte(cfg), 0o644)
	r, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := ParsePorts(r.Tools["npm"].Ports)
	if err != nil || len(ps) != 2 || ps[1] != (Port{3000, 3001}) {
		t.Fatalf("ports %v %v", ps, err)
	}
	os.WriteFile(filepath.Join(dir, RepoFileName), []byte(strings.Replace(cfg, "8080", "0", 1)), 0o644)
	if _, err := LoadRepo(dir); err == nil || !strings.Contains(err.Error(), "tool.npm.ports") {
		t.Fatalf("want validation error, got %v", err)
	}
}
