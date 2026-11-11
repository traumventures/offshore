package adapter

import (
	"path/filepath"

	"github.com/traumventures/offshore/internal/config"
)

// Node handles bun, npm, pnpm and yarn. Installs run remotely: node_modules
// is a machine-scope cache keyed by the lockfile digest and generated on
// the machine by the lockfile's install command, so sessions start with
// dependencies in place. Scripts and tests run remotely too unless
// [tool.X] local lists them ("test", "run", or "run <script>").
type Node struct{}

func (Node) Name() string    { return "node" }
func (Node) Tools() []string { return []string{"bun", "bunx", "npm", "npx", "pnpm", "yarn"} }

func (Node) Detect(root string) bool { return fileExists(filepath.Join(root, "package.json")) }

// nodeLockfiles maps lockfiles to the command that installs exactly them,
// in preference order.
var nodeLockfiles = []struct{ file, tool, install string }{
	{"bun.lock", "bun", "bun install --frozen-lockfile"},
	{"bun.lockb", "bun", "bun install --frozen-lockfile"},
	{"pnpm-lock.yaml", "pnpm", "pnpm install --frozen-lockfile"},
	{"package-lock.json", "npm", "npm ci"},
	{"yarn.lock", "yarn", "yarn install --frozen-lockfile"},
}

// NodeTool returns the package manager the repository at root uses and
// its lockfile ("" when none is found).
func NodeTool(root string) (tool, lockfile, install string) {
	for _, l := range nodeLockfiles {
		if fileExists(filepath.Join(root, l.file)) {
			return l.tool, l.file, l.install
		}
	}
	return "npm", "", ""
}

func (Node) DefaultCaches(root string) []Cache {
	_, lock, install := NodeTool(root)
	if lock == "" {
		return []Cache{{config.Cache{Name: "node-modules", Scope: ScopeMachine, Paths: []string{"node_modules"}},
			"dependencies, shared by the sessions of a machine (no lockfile: not keyed by content)"}}
	}
	return []Cache{{config.Cache{Name: "node-modules", Scope: ScopeMachine, Paths: []string{"node_modules"},
		Digest: []string{"package.json", lock}, Install: install},
		"dependencies per " + lock + " digest, installed on the machine"}}
}

func nodePhase(sub string) PhaseName {
	switch sub {
	case "", "install", "i", "ci", "add", "remove", "rm", "uninstall", "update", "up", "upgrade":
		return Deps
	case "build":
		return Build
	}
	return Run
}

var nodeModules = Artifact{Path: "node_modules", Scope: ScopeMachine}

func (Node) Classify(c Command) Plan {
	sub := subcommand(c.Args)
	key := sub
	if sub == "run" || sub == "run-script" {
		if script := subcommand(c.Args[indexOf(c.Args, sub)+1:]); script != "" {
			// "run <script>" in [tool.X] local selects one script.
			if why, ok := remoteRules(c.Config, c.Tool, sub+" "+script); !ok {
				return Plan{Phases: []Phase{{Name: Run, Where: Local}}, Why: why}
			}
		}
	}
	if c.Tool == "npx" || c.Tool == "bunx" {
		key = ""
	}
	plan := asTyped(c, key, nodePhase(sub), nodeModules)
	if c.Tool == "npx" || c.Tool == "bunx" {
		plan.Phases[0].Name = Run
	}
	return plan
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}
