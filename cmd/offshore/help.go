package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// commandHelp is the --help text of each subcommand (usage lines and a
// short description). Commands whose flags are parsed with a FlagSet
// print their flag defaults below it (see newFlagSet).
var commandHelp = map[string]string{
	"run": `usage: offshore run [--] <tool> [args...]

Run a command through the proxy: remotely on a pool machine when the
repository's .offshore.toml routes the tool there, locally otherwise.
Everything after the tool name belongs to the tool (offshore run cargo --help
shows cargo's help).
`,
	"shim": `usage: offshore shim install [tools...] | status | uninstall | path

  install [tools...]  install proxies (symlinks) into ~/.offshore/bin
                      (default: the [tool.X] entries of .offshore.toml, or cargo)
  status              are the proxies first in PATH (exit 1 if not)
  uninstall           remove the proxies
  path                print the shim directory ($OFFSHORE_SHIM_DIR)
`,
	"status": `usage: offshore status [--json]

Show machines, running sessions and the estimated cost since midnight.
`,
	"why": whyUsage + "\n\nExplain a recent proxied run: placement, timings, labels.\n",
	"top": `usage: offshore top [--once] [--interval 2s] [--width N] [--window 10m]

Live view of machines, sessions, queue and cost.
`,
	"prewarm": `usage: offshore prewarm [--wait] [--force] [--quiet]

Start the remote environment of this repository in the background
([prewarm] run = [...] also warms the build).
`,
	"hooks": `usage: offshore hooks install|uninstall|status

Manage the git post-checkout hook that prewarms new worktrees.
`,
	"metrics": `usage: offshore metrics

Print daemon, pool and machine metrics (Prometheus text format).
`,
	"fetch": `usage: offshore fetch <glob>...

Copy outputs matching the globs from the latest remote build of this
repository into the working tree.
`,
	"outputs": `usage: offshore outputs mount|unmount|status

Mount the remote build directories (e.g. target/) at their local path as a
writable overlay ([outputs] mode = "fuse"): local tools find remote builds
fresh. Default: status.
`,
	"down": `usage: offshore down

Destroy all machines and caches of the pool.
`,
	"reconcile": `usage: offshore reconcile

Reload the pool configuration and apply the policy now.
`,
	"daemon": `usage: offshore daemon run|start|stop|status

  run     run the local daemon in the foreground
  start   start the daemon in the background if it is not running
  stop    stop the daemon
  status  is the daemon running (default; exit 3 if not)
`,
	"env": `usage: offshore env [hash|dockerfile]
       offshore env build [--wait]
       offshore env status [--json]

  (none)      hash and tag of the environment image of this repository
  hash        print the environment hash
  dockerfile  print the generated Dockerfile
  build       build the environment image now (in the background)
  status      where the environment image exists, last prebuild
`,
	"init": `usage: offshore init [--write [--force]] [DIR]

Generate .offshore.toml from the Cargo workspace, CI workflows and
Dockerfiles of the repository (printed unless --write).
`,
	"doctor": `usage: offshore doctor [--json]

Check installation, configuration and connectivity (exit 1 on a failed check).
`,
	"editor": `usage: offshore editor [--mode local|lsp-remote] [vscode|zed|neovim]

Print rust-analyzer settings for remote checks (default editor: vscode;
default mode: [editor] rust_analyzer of .offshore.toml, else local).
  local       rust-analyzer runs cargo check through offshore run
  lsp-remote  build outputs are read from the output FUSE, which this mounts
`,
	"config":       configUsage,
	"controlplane": "usage: offshore controlplane [serve] [flags]\n       offshore controlplane info|client-config|health [flags]\n\nServe a shared control plane, or inspect one (each subcommand accepts --help).\n",
	"sim":          simUsage,
	"report": `usage: offshore report [--since 7d] [--until T] [--by agent|worktree|branch|repo|user] [--json]

Cost, utilisation, queue wait, cache, estimator accuracy and per-label
groups from the event log.
`,
	"controlplane info":          "usage: offshore controlplane info [--state-dir DIR] [--show-key]\n\nURLs, certificate fingerprint and API key location of a control plane.\n",
	"controlplane client-config": "usage: offshore controlplane client-config [flags]\n\nPrint the client [control_plane] block for this control plane.\n",
	"controlplane health":        "usage: offshore controlplane health [flags]\n\nCheck a running control plane (exit status).\n",
	"version":                    "usage: offshore version\n\nPrint the version.\n",
	"help":                       "usage: offshore help [command]\n\nShow the command list, or the help of one command.\n",
	"worker": `usage: offshore worker

Machine worker (internal; started by providers). Configured through
OFFSHORE_* environment variables, which it removes before running sessions.
`,
	"supervisor": `usage: offshore supervisor [--spec FILE] [--state DIR]
       offshore supervisor static [flags]

Supervisor on VM machines (internal; systemd, root).
`,
	"sandbox-init": "usage: offshore sandbox-init\n\nSession namespace init (internal).\n",
	"gocacheprog": `usage: offshore gocacheprog DIR [ENDPOINT]

GOCACHEPROG helper for global [cache.<name>] tool = "go" caches (internal;
set by the worker): serves the Go build cache from DIR and, with ENDPOINT,
the shared result store.
`,
	"debug": `usage: offshore debug token --state-dir DIR --scope probe|session|admin --machine ID [--lease ID] [--ttl 5m]
       offshore debug probe ADDR FINGERPRINT TOKEN
       offshore debug clock [advance DURATION]

Operator tools around machine authentication:
  token  sign a machine token with the pool master key of a state dir
         (or $OFFSHORE_MASTER_KEY)
  probe  print a machine's status
  clock  show or advance the test clock of $OFFSHORE_TEST_CLOCK (offset:FILE)
`,
}

// isHelpFlag reports whether a is -h or --help.
func isHelpFlag(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// wantsHelp reports whether args ask for help before a "--".
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

// flagHelp reports whether the command parses args with a FlagSet that
// prints its own help (usage plus flag defaults).
func flagHelp(cmd string, args []string) bool {
	switch cmd {
	case "top", "prewarm", "init", "report", "sim", "controlplane", "supervisor":
		return true
	case "config":
		return len(args) > 0 && !isHelpFlag(args[0])
	case "debug":
		return len(args) > 0 && args[0] == "token"
	}
	return false
}

// helpRequest returns the help text when cmd args ask for it and the
// command does not handle it itself.
func helpRequest(cmd string, args []string) (string, bool) {
	h, ok := commandHelp[cmd]
	if !ok {
		return "", false
	}
	switch cmd {
	case "run":
		// Only "offshore run --help": later arguments belong to the tool.
		return h, len(args) > 0 && isHelpFlag(args[0])
	case "help":
		return "", false
	}
	if !wantsHelp(args) || flagHelp(cmd, args) {
		return "", false
	}
	return h, true
}

// printHelp writes the help of a command, or the command list.
func printHelp(w io.Writer, cmd string) error {
	if cmd == "" {
		fmt.Fprint(w, usage)
		return nil
	}
	h, ok := commandHelp[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q (see offshore help)", cmd)
	}
	fmt.Fprint(w, h)
	return nil
}

// newFlagSet returns a FlagSet whose -h/--help prints the command's help
// text and flag defaults to stdout. name is the command path ("sim export").
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		h, ok := commandHelp[name]
		if !ok {
			h = commandHelp[strings.Fields(name)[0]]
		}
		fmt.Fprint(os.Stdout, h)
		fmt.Fprintln(os.Stdout, "\nflags:")
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
		fs.SetOutput(nil)
	}
	return fs
}
