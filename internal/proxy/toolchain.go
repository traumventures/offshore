package proxy

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/traumventures/cloudsourced/internal/platform"
	"github.com/traumventures/cloudsourced/internal/proto"
)

// clientToolchain returns the client's toolchain locations (CARGO_HOME and
// RUSTUP_HOME as rustup resolves them).
func clientToolchain() *proto.Toolchain {
	home, _ := os.UserHomeDir()
	abs := func(env, def string) string {
		p := os.Getenv(env)
		if p == "" {
			p = filepath.Join(home, def)
		}
		if a, err := filepath.Abs(p); err == nil {
			p = a
		}
		return p
	}
	return &proto.Toolchain{CargoHome: abs("CARGO_HOME", ".cargo"), RustupHome: abs("RUSTUP_HOME", ".rustup")}
}

// hostTriple is the Rust host triple of this machine (Linux only, like the
// build machines).
func hostTriple() string {
	if platform.Local().Arch == "arm64" {
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
func cargoHomeRustflags(cargoHome string, env map[string]string) map[string]string {
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
	host := hostTriple()
	set("CARGO_BUILD_RUSTFLAGS", cfg.Build.Rustflags)
	set("CARGO_TARGET_"+strings.ToUpper(strings.ReplaceAll(host, "-", "_"))+"_RUSTFLAGS", cfg.Target[host].Rustflags)
	return out
}
