package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/envbuild"
	"github.com/traumventures/offshore/internal/hooks"
	"github.com/traumventures/offshore/internal/labels"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/orchestrator"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/proxy"
)

// prewarmFresh is how long a successful warm-up command counts as done
// for the same commit and environment.
const prewarmFresh = 12 * time.Hour

// prewarmStamp records the last successful prewarm of a repository root.
type prewarmStamp struct {
	Time   time.Time `json:"time"`
	Head   string    `json:"head"`
	Env    string    `json:"env"`
	RepoID string    `json:"repo_id"`
	Run    []string  `json:"run,omitempty"`
}

func prewarmBase(root string) string {
	return filepath.Join(daemon.StateDir(), "prewarm", manifest.HashBytes([]byte(root))[:16])
}

func prewarmLog() string { return filepath.Join(daemon.StateDir(), "prewarm.log") }

// prewarmCmd runs `offshore prewarm [--wait] [--force] [--quiet]`.
func prewarmCmd(args []string) error {
	fs := newFlagSet("prewarm")
	wait := fs.Bool("wait", false, "run in the foreground and wait until the environment is warm")
	force := fs.Bool("force", false, "prewarm even if the environment looks warm")
	quiet := fs.Bool("quiet", false, "print nothing (for git hooks); outside a offshore repository do nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	say := func(format string, a ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "offshore: "+format+"\n", a...)
		}
	}
	if os.Getenv("OFFSHORE_DISABLE") != "" || os.Getenv("OFFSHORE_ACTIVE") != "" {
		say("prewarm skipped (OFFSHORE_DISABLE or inside a remote session)")
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := config.FindRepo(cwd)
	if err != nil {
		if *quiet {
			return nil
		}
		return fmt.Errorf("no %s in %s or its parents", config.RepoFileName, cwd)
	}
	if !*wait {
		return prewarmBackground(root, *force, say)
	}
	repo, err := config.LoadRepo(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(prewarmBase(root)), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(prewarmBase(root)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		say("a prewarm of %s is already running", root)
		return nil
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	c, err := config.LoadClient()
	if err != nil {
		return err
	}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		return err
	}
	repoID := daemon.RepoID(c.User, repo.Root)
	head := labels.HeadCommit(repo.Root)
	if !*force {
		var ms []orchestrator.Machine
		if raw, err := daemon.Call(ctx, daemon.Request{Op: "status"}, nil); err == nil {
			json.Unmarshal(raw, &ms)
		}
		st, _ := readStamp(root)
		if why, ok := alreadyWarm(ms, plan.Hash, repoID, repo.Prewarm.Run, st, head, time.Now()); ok {
			say("already warm: %s", why)
			return nil
		}
	}
	argv := repo.Prewarm.Run
	if len(argv) == 0 {
		argv = []string{"true"} // lease, start and sync only
	}
	os.Unsetenv("OFFSHORE_PORTS") // never forward ports for a prewarm
	start := time.Now()
	say("prewarming %s (%v)", root, argv)
	code, err := proxy.Prewarm(ctx, repo, argv, os.Stderr)
	if err != nil {
		return fmt.Errorf("prewarm: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("prewarm: %v exited with %d", argv, code)
	}
	writeStamp(root, prewarmStamp{Time: time.Now(), Head: head, Env: plan.Hash, RepoID: repoID, Run: repo.Prewarm.Run})
	say("warm after %s", time.Since(start).Round(time.Millisecond))
	return nil
}

// prewarmBackground starts `prewarm --wait` detached, logging to
// prewarm.log, and returns at once.
func prewarmBackground(root string, force bool, say func(string, ...any)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(daemon.StateDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(prewarmLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "=== %s prewarm %s\n", time.Now().Format(time.RFC3339), root)
	args := []string{"prewarm", "--wait"}
	if force {
		args = append(args, "--force")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Process.Release()
	say("prewarming %s in the background (log: %s)", root, prewarmLog())
	return nil
}

// alreadyWarm decides whether a prewarm can be skipped: a running, ready
// machine of the environment holds an unexpired build slot of the
// repository and, when a warm-up command is configured, it already
// succeeded for the current commit and environment recently.
func alreadyWarm(ms []orchestrator.Machine, envHash, repoID string, run []string, st *prewarmStamp, head string, now time.Time) (string, bool) {
	var warm string
	for _, m := range ms {
		if !m.Removed.IsZero() || m.State != provider.Running || m.Status == nil || !m.Status.Ready || m.Status.Draining {
			continue
		}
		if m.EnvHash == envHash && slices.Contains(m.Status.WarmRepos, repoID) {
			warm = m.ID
			break
		}
	}
	if warm == "" {
		return "", false
	}
	if len(run) == 0 {
		return "machine " + warm + " has a warm build slot", true
	}
	if st != nil && st.Env == envHash && st.RepoID == repoID && head != "" && st.Head == head &&
		slices.Equal(st.Run, run) && now.Sub(st.Time) < prewarmFresh {
		return fmt.Sprintf("machine %s has a warm build slot; %v ran for this commit %s ago", warm, run, now.Sub(st.Time).Round(time.Second)), true
	}
	return "", false
}

func readStamp(root string) (*prewarmStamp, error) {
	b, err := os.ReadFile(prewarmBase(root) + ".json")
	if err != nil {
		return nil, err
	}
	var st prewarmStamp
	return &st, json.Unmarshal(b, &st)
}

func writeStamp(root string, st prewarmStamp) {
	b, _ := json.Marshal(st)
	os.WriteFile(prewarmBase(root)+".json", b, 0o600)
}

// hooksCmd runs `offshore hooks install|uninstall|status`.
func hooksCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: offshore hooks install|uninstall|status")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir, err := hooks.Dir(cwd)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, hooks.Name)
	switch args[0] {
	case "install":
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		if r, err := filepath.EvalSymlinks(bin); err == nil {
			bin = r
		}
		chained, err := hooks.Install(dir, bin)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s (prewarm on checkout and new worktrees)\n", path)
		if chained {
			fmt.Printf("the previous hook runs first: %s\n", filepath.Join(dir, hooks.Chain))
		}
	case "uninstall":
		if err := hooks.Uninstall(dir); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", path)
	case "status":
		if hooks.Installed(dir) {
			fmt.Printf("installed: %s\n", path)
		} else {
			fmt.Printf("not installed (%s)\n", path)
		}
	default:
		return errors.New("usage: offshore hooks install|uninstall|status")
	}
	return nil
}
