package labels

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeRepo lays out a main checkout "app" on branch main and a linked
// worktree on branch wp/x, as git does.
func fakeRepo(t *testing.T) (main, wt string) {
	d := t.TempDir()
	main = filepath.Join(d, "app")
	wt = filepath.Join(d, "wt", "x")
	write(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(main, ".git", "refs", "heads", "main"), "aaaa\n")
	write(t, filepath.Join(main, ".git", "packed-refs"), "# pack\nbbbb refs/heads/wp/x\n")
	wg := filepath.Join(main, ".git", "worktrees", "x")
	write(t, filepath.Join(wg, "HEAD"), "ref: refs/heads/wp/x\n")
	write(t, filepath.Join(wg, "commondir"), "../..\n")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+wg+"\n")
	return main, wt
}

func TestDetectGitAndAgent(t *testing.T) {
	main, wt := fakeRepo(t)
	l := Detect(main, "alice", env(nil))
	if l[Worktree] != main || l[Branch] != "main" || l[Repo] != "app" || l[User] != "alice" || l[Agent] != "" {
		t.Fatalf("main checkout: %v", l)
	}
	l = Detect(wt, "alice", env(map[string]string{"CODEX_THREAD_ID": "th-1", "OPENAI_API_KEY": "sk-secret", "CLAUDECODE": "1"}))
	if l[Worktree] != wt || l[Branch] != "wp/x" || l[Repo] != "app" || l[Agent] != "codex" || l[AgentSession] != "th-1" {
		t.Fatalf("worktree: %v", l)
	}
	for _, v := range l {
		if strings.Contains(v, "secret") {
			t.Fatalf("secret leaked into labels: %v", l)
		}
	}
	if HeadCommit(main) != "aaaa" || HeadCommit(wt) != "bbbb" {
		t.Fatalf("head %q %q", HeadCommit(main), HeadCommit(wt))
	}
	// Explicit agent and free labels win; invalid keys are dropped.
	l = Detect(wt, "", env(map[string]string{"CODEX_THREAD_ID": "th-1", EnvAgent: "my-bot", EnvLabels: "task=WP-7, Bad=x ,team = infra,noeq"}))
	if l[Agent] != "my-bot" || l[AgentSession] != "" || l["task"] != "WP-7" || l["team"] != "infra" || l["Bad"] != "" || l[User] != "" {
		t.Fatalf("override: %v", l)
	}
	if l := Detect(t.TempDir(), "", env(map[string]string{"CLAUDECODE": "1"})); l[Agent] != "claude-code" || l[Branch] != "" {
		t.Fatalf("no git: %v", l)
	}
}

func TestCleanLimits(t *testing.T) {
	in := map[string]string{"agent": "a\x1bb", "empty": " ", "long": strings.Repeat("x", 500)}
	for i := 0; i < 30; i++ {
		in[string(rune('a'+i%26))+"k"+string(rune('a'+i/26))] = "v"
	}
	c := Clean(in)
	if len(c) != MaxLabels || c["agent"] != "ab" || c["empty"] != "" {
		t.Fatalf("clean: %d %v", len(c), c)
	}
	if Clean(map[string]string{"X": "y"}) != nil {
		t.Fatal("want nil")
	}
	if got := Format(map[string]string{"zz": "1", "agent": "codex", "worktree": "/w"}); got != "worktree=/w agent=codex zz=1" {
		t.Fatalf("format %q", got)
	}
}
