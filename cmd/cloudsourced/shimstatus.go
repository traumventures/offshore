package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// shimReport describes whether the proxies in the shim directory are what
// the shell runs.
type shimReport struct {
	Dir      string
	Tools    []string          // installed proxies (symlinks to cloudsourced)
	Position int               // 1-based position of Dir in PATH, 0 = absent
	Shadowed map[string]string // tool -> binary found earlier in PATH
}

// Active reports whether every installed proxy wins the PATH lookup.
func (r shimReport) Active() bool {
	return len(r.Tools) > 0 && r.Position > 0 && len(r.Shadowed) == 0
}

// inspectShims inspects dir against path; self is the resolved
// cloudsourced executable.
func inspectShims(dir, path, self string) shimReport {
	r := shimReport{Dir: dir, Shadowed: map[string]string{}}
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if t, err := filepath.EvalSymlinks(filepath.Join(dir, de.Name())); err == nil && t == self {
			r.Tools = append(r.Tools, de.Name())
		}
	}
	sort.Strings(r.Tools)
	cleanDir := filepath.Clean(dir)
	entries := filepath.SplitList(path)
	for i, e := range entries {
		if e != "" && filepath.Clean(e) == cleanDir {
			r.Position = i + 1
			break
		}
	}
	for _, t := range r.Tools {
		for i, e := range entries {
			if r.Position > 0 && i+1 >= r.Position {
				break
			}
			if e == "" {
				e = "."
			}
			cand := filepath.Join(e, t)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				if real, err := filepath.EvalSymlinks(cand); err != nil || real != self {
					r.Shadowed[t] = cand
					break
				}
			}
		}
	}
	return r
}

func (r shimReport) print(w io.Writer) {
	home, _ := os.UserHomeDir()
	short := r.Dir
	if home != "" && strings.HasPrefix(short, home+"/") {
		short = "~" + strings.TrimPrefix(short, home)
	}
	if len(r.Tools) == 0 {
		fmt.Fprintf(w, "no shims installed in %s (cloudsourced shim install)\n", short)
		return
	}
	fmt.Fprintf(w, "shims in %s: %s\n", short, strings.Join(r.Tools, ", "))
	switch {
	case r.Position == 0:
		fmt.Fprintf(w, "%s is not in PATH: the shims are inactive\n", short)
	case r.Position == 1:
		fmt.Fprintf(w, "%s is first in PATH\n", short)
	default:
		fmt.Fprintf(w, "%s is entry %d in PATH, not first\n", short, r.Position)
	}
	for _, t := range r.Tools {
		if p, ok := r.Shadowed[t]; ok {
			fmt.Fprintf(w, "%s runs %s, not the proxy\n", t, p)
		}
	}
	if r.Active() {
		fmt.Fprintln(w, "active: these tools run through cloudsourced")
		return
	}
	fmt.Fprintf(w, "\nPut the shims first in PATH (e.g. in ~/.profile, ~/.zshrc):\n  export PATH=\"%s:$PATH\"\n", r.Dir)
}

// shimStatus prints the shim report and fails when the shims are not
// active, so scripts can check the setup.
func shimStatus() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	r := inspectShims(ShimDir(), os.Getenv("PATH"), self)
	r.print(os.Stdout)
	if !r.Active() {
		return exitCode(1)
	}
	return nil
}
