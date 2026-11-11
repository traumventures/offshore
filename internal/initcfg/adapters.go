package initcfg

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/traumventures/cloudsourced/internal/adapter"
)

// detectAdapters records the ecosystems of the repository and merges
// their adapters' default caches and tool sections.
func (d *Detection) detectAdapters() {
	seen := map[string]bool{}
	for _, a := range adapter.All {
		found := a.Detect(d.Root)
		if a.Name() == "cargo" {
			found = d.Rust // the workspace may live below the root
		}
		if !found {
			continue
		}
		d.Adapters = append(d.Adapters, a.Name())
		for _, c := range a.DefaultCaches(d.Root) {
			if a.Name() == "cargo" && c.Name == "target" {
				c.Paths = []string{filepath.ToSlash(filepath.Join(d.Workspace, "target"))}
			}
			if seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			d.Caches = append(d.Caches, c)
		}
		switch a.Name() {
		case "go":
			d.Tools = append(d.Tools, ToolSection{Name: "go", Comment: "go build/vet remotely; go test builds remotely and runs locally",
				Env: []string{"GOFLAGS", "GOPRIVATE", "GOEXPERIMENT", "CGO_ENABLED"}})
		case "node":
			tool, lock, _ := adapter.NodeTool(d.Root)
			why := "installs run remotely into the machine's node_modules cache"
			if lock == "" {
				why += " (no lockfile found)"
			}
			d.Tools = append(d.Tools, ToolSection{Name: tool, Comment: why + "; scripts and tests remotely (list them in local to run here)",
				Env: []string{"NODE_ENV", "NODE_OPTIONS"}})
		}
	}
}

func (d *Detection) has(name string) bool { return slices.Contains(d.Adapters, name) }

var goDirective = regexp.MustCompile(`^go\s+(\d+\.\d+)`)

// goImage picks the official Go image for go.mod's go directive.
func goImage(root string) string {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if m := goDirective.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
				return "golang:" + m[1] + "-trixie"
			}
		}
	}
	return "golang:trixie"
}

// nodeImage picks the image of the repository's package manager.
func nodeImage(root string) string {
	if tool, _, _ := adapter.NodeTool(root); tool == "bun" {
		return "oven/bun:1-debian"
	}
	return "node:lts-trixie-slim"
}
