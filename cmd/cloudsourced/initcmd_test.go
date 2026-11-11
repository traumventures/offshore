package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traumventures/cloudsourced/internal/config"
)

func TestInitCmd(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../internal/initcfg/testdata/app")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := initCmd([]string{dir}, &out); err != nil || !strings.Contains(out.String(), `rust = "app/rust-toolchain.toml"`) {
		t.Fatalf("print: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, config.RepoFileName)); err == nil {
		t.Fatal("printing must not write")
	}
	out.Reset()
	if err := initCmd([]string{"--write", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadRepo(dir); err != nil {
		t.Fatal(err)
	}
	if err := initCmd([]string{"--write", dir}, &out); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite without --force: %v", err)
	}
	if err := initCmd([]string{"--write", "--force", dir}, &out); err != nil {
		t.Fatal(err)
	}
}
