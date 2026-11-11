package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallChainUninstall(t *testing.T) {
	d := t.TempDir()
	log := filepath.Join(d, "calls")
	// A fake offshore binary records its arguments.
	bin := filepath.Join(d, "cs")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"cs $*\" >> "+log+"\n"), 0o755)
	hooksDir := filepath.Join(d, "hooks")
	os.MkdirAll(hooksDir, 0o755)
	foreign := "#!/bin/sh\necho \"old $*\" >> " + log + "\nexit 3\n"
	os.WriteFile(filepath.Join(hooksDir, Name), []byte(foreign), 0o755)

	chained, err := Install(hooksDir, bin)
	if err != nil || !chained || !Installed(hooksDir) {
		t.Fatalf("install: chained=%v err=%v", chained, err)
	}
	// Idempotent: a second install rewrites ours and keeps the chain.
	if chained, err := Install(hooksDir, bin); err != nil || chained {
		t.Fatalf("reinstall: chained=%v err=%v", chained, err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooksDir, Chain)); string(b) != foreign {
		t.Fatalf("chain lost: %q", b)
	}
	run := func(args ...string) int {
		cmd := exec.Command(filepath.Join(hooksDir, Name), args...)
		cmd.Env = append(os.Environ(), "OFFSHORE_DISABLE=")
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	// Branch checkout: old hook first (its status is kept), then prewarm.
	if code := run("a", "b", "1"); code != 3 {
		t.Fatalf("exit %d, want the chained hook's 3", code)
	}
	// File checkout: no prewarm.
	run("a", "b", "0")
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "old a b 1\ncs prewarm --quiet\nold a b 0" {
		t.Fatalf("calls:\n%s", got)
	}

	if err := Uninstall(hooksDir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooksDir, Name)); string(b) != foreign {
		t.Fatalf("not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, Chain)); !os.IsNotExist(err) {
		t.Fatal("chain file left behind")
	}
	if err := Uninstall(hooksDir); err == nil {
		t.Fatal("uninstall of a foreign hook must fail")
	}
}

func TestInstallFreshAndDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	dir, err := Dir(repo)
	if err != nil || dir != filepath.Join(repo, ".git", "hooks") {
		t.Fatalf("dir %q %v", dir, err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	bin := filepath.Join(t.TempDir(), "cs")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$(pwd) $*\" >> "+log+"\n"), 0o755)
	if chained, err := Install(dir, bin); err != nil || chained {
		t.Fatal(chained, err)
	}
	// git worktree add runs post-checkout in the new worktree.
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Env = append(os.Environ(), "OFFSHORE_DISABLE=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-q", "-b", "feature", wt)
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != wt+" prewarm --quiet" {
		t.Fatalf("hook calls %q, want prewarm in %s", got, wt)
	}
	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Name)); !os.IsNotExist(err) {
		t.Fatal("hook left behind")
	}
}
