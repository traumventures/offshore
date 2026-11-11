// Package hooks installs the git post-checkout hook that prewarms the
// remote environment when a branch is checked out or a worktree is created
// (`git worktree add` runs post-checkout in the new worktree).
//
// The hook is chained: an existing post-checkout hook that is not ours is
// renamed to post-checkout.cloudsourced-chain and still runs first, with
// its exit status preserved. Uninstall restores it.
package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the hook file; Chain the renamed previous hook.
const (
	Name   = "post-checkout"
	Chain  = "post-checkout.cloudsourced-chain"
	Marker = "# managed by `cloudsourced hooks install`"
)

// Script returns the hook for the cloudsourced binary bin.
func Script(bin string) string {
	return `#!/bin/sh
` + Marker + `
# Prewarms the remote build environment after branch checkouts and new
# worktrees (non-blocking). Remove with: cloudsourced hooks uninstall
status=0
chain="$(dirname "$0")/` + Chain + `"
if [ -x "$chain" ]; then
	"$chain" "$@" || status=$?
fi
# $3 is 1 for branch checkouts (including git worktree add), 0 for files.
if [ "$3" = "1" ] && [ -z "$CLOUDSOURCED_DISABLE" ]; then
	cs=` + shellQuote(bin) + `
	[ -x "$cs" ] || cs=$(command -v cloudsourced 2>/dev/null)
	if [ -n "$cs" ]; then
		"$cs" prewarm --quiet </dev/null >/dev/null 2>&1 || true
	fi
fi
exit $status
`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Dir returns the hooks directory of the git checkout containing dir
// (honours core.hooksPath; shared by all worktrees of a repository).
func Dir(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

// Installed reports whether hooksDir holds our hook.
func Installed(hooksDir string) bool {
	b, err := os.ReadFile(filepath.Join(hooksDir, Name))
	return err == nil && bytes.Contains(b, []byte(Marker))
}

// Install writes the hook into hooksDir. It is idempotent (an installed
// hook is rewritten with the current binary) and chains a foreign hook.
// It reports whether a foreign hook was chained by this call.
func Install(hooksDir, bin string) (chained bool, err error) {
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(hooksDir, Name)
	if _, err := os.Lstat(path); err == nil && !Installed(hooksDir) {
		if _, err := os.Lstat(filepath.Join(hooksDir, Chain)); err == nil {
			return false, fmt.Errorf("%s exists and is not ours, and %s exists too; resolve manually", path, Chain)
		}
		if err := os.Rename(path, filepath.Join(hooksDir, Chain)); err != nil {
			return false, err
		}
		chained = true
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(Script(bin)), 0o755); err != nil {
		return chained, err
	}
	return chained, os.Rename(tmp, path)
}

// Uninstall removes our hook and restores a chained one. A foreign hook
// is left alone (error).
func Uninstall(hooksDir string) error {
	path := filepath.Join(hooksDir, Name)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !Installed(hooksDir) {
		return fmt.Errorf("%s was not installed by cloudsourced; left unchanged", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	chain := filepath.Join(hooksDir, Chain)
	if _, err := os.Lstat(chain); err == nil {
		return os.Rename(chain, path)
	}
	return nil
}
