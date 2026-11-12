package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Linked worktrees of a repository share its family; another repository
// or user does not.
func TestRepoFamilyAcrossWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	main, wt, other := filepath.Join(dir, "main"), filepath.Join(dir, "wt"), filepath.Join(dir, "other")
	run := func(d string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = d
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for _, d := range []string{main, other} {
		os.MkdirAll(d, 0o755)
		run(d, "init", "-q")
		os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644)
		run(d, "add", "f")
		run(d, "commit", "-qm", "init")
	}
	run(main, "worktree", "add", "-q", wt)
	f := RepoFamily("u", main)
	if f == "" || RepoFamily("u", wt) != f {
		t.Fatalf("worktrees: %q %q", f, RepoFamily("u", wt))
	}
	if RepoFamily("u", other) == f || RepoFamily("v", main) == f {
		t.Fatal("other repository or user in the family")
	}
	if RepoFamily("u", dir) != "" {
		t.Fatal("not a git worktree")
	}
	if RepoID("u", main) == RepoID("u", wt) {
		t.Fatal("worktrees share a repository id (slots and sources must stay per worktree)")
	}
}
