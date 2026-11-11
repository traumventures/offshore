package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/sandbox"
)

// Toolchain relocation (proto.Exec.Toolchain, [outputs] mode = "fuse").
//
// Cargo fingerprints contain the absolute paths of registry sources
// ($CARGO_HOME/registry/src/...) and the configured linker. For local cargo
// to find the remote build directory fresh, sessions see the image's
// CARGO_HOME and RUSTUP_HOME at the client's paths (bind mounts in the
// session's mount namespace) and select the linker through a "cc" on PATH
// instead of CARGO_TARGET_<triple>_LINKER, which cargo would hash.

// relocation is what a session needs to see the toolchain at the client's
// paths.
type relocation struct {
	binds []sandbox.Bind
	env   [][2]string // CARGO_HOME, RUSTUP_HOME, PATH
}

// dirUsable reports whether a bind mount onto p hides nothing: p is
// missing or an empty directory.
func dirUsable(p string) bool {
	des, err := os.ReadDir(p)
	if os.IsNotExist(err) {
		_, lerr := os.Lstat(p)
		return os.IsNotExist(lerr)
	}
	return err == nil && len(des) == 0
}

// relocateToolchain computes the relocation of the image toolchain
// (imageEnv: the worker's environment) to tc. roots are the projected
// roots of the session, which a bind must neither cover nor enter.
// usable reports whether a bind may cover a path (dirUsable).
func relocateToolchain(imageEnv map[string]string, tc *proto.Toolchain, roots []string, usable func(string) bool) relocation {
	var r relocation
	if tc == nil {
		return r
	}
	home := imageEnv["HOME"]
	if home == "" {
		home = "/root"
	}
	path := imageEnv["PATH"]
	one := func(envName, def, client string) (string, bool) {
		img := imageEnv[envName]
		if img == "" {
			img = filepath.Join(home, def)
		}
		img = filepath.Clean(img)
		if fi, err := os.Stat(img); err != nil || !fi.IsDir() {
			return "", false // no toolchain of this kind in the image
		}
		client = filepath.Clean(client)
		if !filepath.IsAbs(client) || strings.Count(client, "/") < 2 {
			return "", false
		}
		if client != img {
			for _, root := range append([]string{img}, roots...) {
				if strings.HasPrefix(root+"/", client+"/") || strings.HasPrefix(client+"/", root+"/") {
					return "", false
				}
			}
			if !usable(client) {
				return "", false
			}
			r.binds = append(r.binds, sandbox.Bind{Src: img, Dst: client})
		}
		r.env = append(r.env, [2]string{envName, client})
		return img, true
	}
	if img, ok := one("CARGO_HOME", ".cargo", tc.CargoHome); ok {
		bin, cbin := filepath.Join(img, "bin"), filepath.Join(filepath.Clean(tc.CargoHome), "bin")
		parts := strings.Split(path, ":")
		found := false
		for i, p := range parts {
			if filepath.Clean(p) == bin {
				parts[i], found = cbin, true
			}
		}
		if !found {
			parts = append([]string{cbin}, parts...)
		}
		r.env = append(r.env, [2]string{"PATH", strings.Join(parts, ":")})
	}
	one("RUSTUP_HOME", ".rustup", tc.RustupHome)
	return r
}

func (s *Worker) imageEnv() map[string]string {
	m := map[string]string{}
	for _, kv := range s.env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (s *Worker) relocation(req proto.Exec) relocation {
	roots := make([]string, 0, len(req.Roots))
	for r := range req.Roots {
		roots = append(roots, r)
	}
	return relocateToolchain(s.imageEnv(), req.Toolchain, roots, dirUsable)
}

// linkerShim returns a directory with a "cc" that links with linker. rustc
// calls the C compiler driver "cc" from PATH as linker; compilations (-c)
// pass through unchanged.
func linkerShim(linker string) (string, error) {
	real, err := exec.LookPath("cc")
	if err != nil {
		return "", fmt.Errorf("linker %s: %w", linker, err)
	}
	dir := filepath.Join(os.TempDir(), "offshore-linker-"+linker)
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" -c \"*|*\" -E \"*|*\" -S \"*) exec %s \"$@\";; esac\nexec %s \"$@\" -fuse-ld=%s\n", real, real, linker)
	cc := filepath.Join(dir, "cc")
	if b, err := os.ReadFile(cc); err == nil && string(b) == script {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := cc + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, cc)
}

// validLinker guards the shim's file name.
func validLinker(l string) bool { return l == config.LinkerMold || l == config.LinkerLLD }
