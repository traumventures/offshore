package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/daemon"
)

// outputsCmd manages the output FUSE: the remote build directories of the
// current repository mounted read-only at their local path.
func outputsCmd(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	switch sub {
	case "mount", "unmount":
		root, err := config.FindRepo(".")
		if err != nil {
			return fmt.Errorf("no %s found", config.RepoFileName)
		}
		raw, err := daemon.Call(ctx, daemon.Request{Op: "outputs-" + sub, Root: root}, os.Stderr)
		if err != nil {
			return err
		}
		if sub == "mount" {
			var mps []string
			json.Unmarshal(raw, &mps)
			for _, mp := range mps {
				fmt.Println("mounted", mp)
			}
		} else {
			fmt.Println("unmounted outputs of", root)
		}
		return nil
	case "status":
		raw, err := daemon.Call(ctx, daemon.Request{Op: "outputs-status"}, os.Stderr)
		if err != nil {
			return err
		}
		var st []daemon.OutputsStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		if len(st) == 0 {
			fmt.Println("no repositories with remote outputs")
		}
		for _, s := range st {
			mounted := "not mounted"
			if len(s.Mountpoints) > 0 {
				mounted = "mounted at " + strings.Join(s.Mountpoints, ", ")
			}
			machine := s.Machine
			if machine == "" {
				machine = "-"
			}
			fmt.Printf("%s: %s (latest build on %s, generation %d, %d cached directories, %d known files)\n",
				s.Root, mounted, machine, s.Generation, s.CachedDirs, s.KnownFiles)
		}
		return nil
	}
	return errors.New("usage: cloudsourced outputs mount|unmount|status")
}

// editorCmd prints rust-analyzer settings that run flycheck and build
// scripts remotely. rust-analyzer itself always runs locally.
//
//   - local: proc-macro libraries and build script outputs come back through
//     eager outputs.
//   - lsp-remote: they are read from the output FUSE (the build directory
//     mounted at its local path), which this command mounts.
func editorCmd(args []string) error {
	editor, mode := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--mode" && i+1 < len(args):
			mode = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode="):
			mode = strings.TrimPrefix(a, "--mode=")
		case editor == "":
			editor = a
		default:
			return errors.New("usage: cloudsourced editor [--mode local|lsp-remote] [vscode|zed|neovim]")
		}
	}
	if editor == "" {
		editor = "vscode"
	}
	root, rootErr := config.FindRepo(".")
	if mode == "" {
		mode = config.RustAnalyzerLocal
		if rootErr == nil {
			if repo, err := config.LoadRepo(root); err == nil {
				mode = repo.Editor.RustAnalyzer
			}
		}
	}
	if mode != config.RustAnalyzerLocal && mode != config.RustAnalyzerLSPRemote {
		return fmt.Errorf("unknown rust-analyzer mode %q (local, lsp-remote)", mode)
	}
	self, _ := os.Executable()
	check := []string{self, "run", "cargo", "check", "--workspace", "--all-targets", "--message-format=json"}
	q := func(xs []string) string {
		b, _ := json.Marshal(xs)
		return string(b)
	}
	lua := func(xs []string) string { return strings.NewReplacer("[", "{", "]", "}").Replace(q(xs)) }
	switch editor {
	case "vscode":
		fmt.Printf(`// .vscode/settings.json
{
  "rust-analyzer.check.overrideCommand": %s,
  "rust-analyzer.cargo.buildScripts.overrideCommand": %s,
  "rust-analyzer.procMacro.enable": true
}
`, q(check), q(check))
	case "zed":
		fmt.Printf(`// .zed/settings.json
{
  "lsp": { "rust-analyzer": { "initialization_options": {
    "check": { "overrideCommand": %s },
    "cargo": { "buildScripts": { "overrideCommand": %s } },
    "procMacro": { "enable": true }
  } } }
}
`, q(check), q(check))
	case "neovim":
		fmt.Printf(`-- rust-analyzer settings (lspconfig)
settings = { ["rust-analyzer"] = {
  check = { overrideCommand = %s },
  cargo = { buildScripts = { overrideCommand = %s } },
  procMacro = { enable = true },
} }
`, lua(check), lua(check))
	default:
		return fmt.Errorf("unknown editor %q (vscode, zed, neovim)", editor)
	}
	if mode == config.RustAnalyzerLocal {
		fmt.Print(`
# Proc macros and build script outputs must exist locally for rust-analyzer.
# Add them to the eager outputs in .cloudsourced.toml:
#   [tool.cargo]
#   eager = ["target/debug/deps/*.so", "target/debug/build/*/out/**"]
# or use the output FUSE: cloudsourced editor --mode lsp-remote
`)
		return nil
	}
	fmt.Print(`
# lsp-remote: proc macros and build script outputs are read from the
# remote build directory, mounted read-only at its local path (output FUSE).
# Keep it mounted with [outputs] mode = "fuse" in .cloudsourced.toml.
# The local toolchain must match the remote one (rust-toolchain.toml), or
# rust-analyzer cannot load the proc-macro libraries.
`)
	if rootErr != nil {
		fmt.Fprintln(os.Stderr, "cloudsourced: not in a repository, outputs not mounted")
		return nil
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "outputs-mount", Root: root}, os.Stderr)
	if err != nil {
		return fmt.Errorf("mount outputs: %w", err)
	}
	var mps []string
	json.Unmarshal(raw, &mps)
	for _, mp := range mps {
		fmt.Fprintln(os.Stderr, "cloudsourced: outputs mounted at", mp)
	}
	if len(mps) > 0 {
		fmt.Fprintf(os.Stderr, "cloudsourced: the mount is read-only; local cargo builds need \"cloudsourced outputs unmount\" first (%s)\n", filepath.Base(mps[0]))
	}
	return nil
}
