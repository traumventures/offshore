package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveRef reads a secret reference: "env:VAR", "file:PATH" (relative to
// dir, "~/" expands to the home directory) or "cmd:COMMAND" (run by /bin/sh
// in dir). Values are trimmed of trailing newlines.
func ResolveRef(ref, dir string) (string, error) {
	kind, arg, _ := strings.Cut(ref, ":")
	switch kind {
	case "env":
		v, ok := os.LookupEnv(arg)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", arg)
		}
		return v, nil
	case "file":
		p := arg
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(os.Getenv("HOME"), p[2:])
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		return strings.TrimRight(string(b), "\r\n"), err
	case "cmd":
		cmd := exec.Command("/bin/sh", "-c", arg)
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("%q failed: %w", arg, err)
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	return "", fmt.Errorf("unsupported reference %q (env:, file:, cmd:)", ref)
}
