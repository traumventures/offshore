package adapter

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// OutputAlignment makes the outputs of a remote build fresh for the local
// tool when they appear locally through the output overlay ([outputs] mode
// = "fuse"): the remote session mirrors the client's tool homes at their
// local paths and gets variables the tool fingerprints.
type OutputAlignment struct {
	// CargoHome and RustupHome are the client's tool homes, mounted at the
	// same paths in the session ("" = none).
	CargoHome, RustupHome string
	// Env is added to the remote environment (forwarded variables win).
	Env map[string]string
}

// AlignOutputs returns the output alignment of the plan's tool for a remote
// phase with environment env, or false when the tool needs none.
func (p Plan) AlignOutputs(env map[string]string) (OutputAlignment, bool) {
	if p.align == nil {
		return OutputAlignment{}, false
	}
	return p.align(env), true
}

// cargoAlign: cargo hashes paths below CARGO_HOME/RUSTUP_HOME (registry
// sources, the toolchain's sysroot) and the effective rustflags into every
// fingerprint, so the session uses the client's homes and the rustflags of
// the client's $CARGO_HOME/config.toml.
func cargoAlign(c Command) func(env map[string]string) OutputAlignment {
	return func(env map[string]string) OutputAlignment {
		cargoHome, rustupHome := clientHomes(c)
		return OutputAlignment{CargoHome: cargoHome, RustupHome: rustupHome, Env: cargoHomeRustflags(cargoHome, hostTriple(c), env)}
	}
}

// clientHomes returns CARGO_HOME and RUSTUP_HOME as rustup resolves them.
func clientHomes(c Command) (cargoHome, rustupHome string) {
	home := c.getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	abs := func(env, def string) string {
		p := c.getenv(env)
		if p == "" {
			p = filepath.Join(home, def)
		}
		if a, err := filepath.Abs(p); err == nil {
			p = a
		}
		return p
	}
	return abs("CARGO_HOME", ".cargo"), abs("RUSTUP_HOME", ".rustup")
}

// hostTriple is the Rust host triple of this machine (Linux only, like the
// build machines).
func hostTriple(c Command) string {
	if c.host().Arch == "arm64" {
		return "aarch64-unknown-linux-gnu"
	}
	return "x86_64-unknown-linux-gnu"
}

// cargoHomeRustflags returns rustflags variables for the rustflags set in
// the client's $CARGO_HOME/config.toml. That file is not projected to the
// build machine, but cargo hashes the effective rustflags into every
// fingerprint: without them, local cargo would rebuild everything the
// remote build produced. Variables already set (forwarded environment)
// win; repository .cargo/config.toml files are projected anyway.
func cargoHomeRustflags(cargoHome, host string, env map[string]string) map[string]string {
	b, err := os.ReadFile(filepath.Join(cargoHome, "config.toml"))
	if err != nil {
		if b, err = os.ReadFile(filepath.Join(cargoHome, "config")); err != nil {
			return nil
		}
	}
	var cfg struct {
		Build struct {
			Rustflags any `toml:"rustflags"`
		} `toml:"build"`
		Target map[string]struct {
			Rustflags any `toml:"rustflags"`
		} `toml:"target"`
	}
	if toml.Unmarshal(b, &cfg) != nil {
		return nil
	}
	for _, k := range []string{"RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS"} {
		if _, ok := env[k]; ok {
			return nil // takes precedence over every config file
		}
	}
	out := map[string]string{}
	set := func(key string, v any) {
		if _, ok := env[key]; ok {
			return
		}
		switch v := v.(type) {
		case string:
			out[key] = v
		case []any:
			var parts []string
			for _, x := range v {
				s, ok := x.(string)
				if !ok || strings.ContainsAny(s, " \t") {
					return // not expressible as a space separated variable
				}
				parts = append(parts, s)
			}
			out[key] = strings.Join(parts, " ")
		}
	}
	set("CARGO_BUILD_RUSTFLAGS", cfg.Build.Rustflags)
	set("CARGO_TARGET_"+strings.ToUpper(strings.ReplaceAll(host, "-", "_"))+"_RUSTFLAGS", cfg.Target[host].Rustflags)
	return out
}
