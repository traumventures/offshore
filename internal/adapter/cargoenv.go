package adapter

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// configEnvVar is one entry of cargo's [env] configuration.
type configEnvVar struct {
	key, value string
	force      bool // override a variable already set in the environment
}

// cargoConfigEnv reads cargo's [env] configuration as cargo resolves it
// for a command in cwd: .cargo/config.toml (or .cargo/config) in cwd and
// every parent, then $CARGO_HOME/config.toml; nearer files win.
// `relative = true` values are paths relative to the directory containing
// the .cargo directory.
func cargoConfigEnv(cwd string, getenv func(string) string) []configEnvVar {
	var files []string // nearest first
	for d := cwd; ; d = filepath.Dir(d) {
		files = append(files, configFile(filepath.Join(d, ".cargo")))
		if filepath.Dir(d) == d {
			break
		}
	}
	home := getenv("CARGO_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".cargo")
		}
	}
	if home != "" {
		files = append(files, configFile(home))
	}
	vars := map[string]configEnvVar{}
	seenFile := map[string]bool{}
	for _, f := range files {
		if f == "" || seenFile[f] {
			continue
		}
		seenFile[f] = true
		for _, v := range readConfigEnv(f) {
			if _, ok := vars[v.key]; !ok {
				vars[v.key] = v
			}
		}
	}
	out := make([]configEnvVar, 0, len(vars))
	for _, v := range vars {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// configFile returns dir/config.toml or dir/config, "" if neither exists.
func configFile(dir string) string {
	for _, n := range []string{"config.toml", "config"} {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func readConfigEnv(file string) []configEnvVar {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var cfg struct {
		Env map[string]any `toml:"env"`
	}
	if toml.Unmarshal(b, &cfg) != nil {
		return nil
	}
	base := filepath.Dir(filepath.Dir(file)) // the directory containing .cargo
	var out []configEnvVar
	for k, raw := range cfg.Env {
		switch v := raw.(type) {
		case string:
			out = append(out, configEnvVar{key: k, value: v})
		case map[string]any:
			val, ok := v["value"].(string)
			if !ok {
				continue
			}
			force, _ := v["force"].(bool)
			if rel, _ := v["relative"].(bool); rel && !filepath.IsAbs(val) {
				val = filepath.Join(base, val)
			}
			out = append(out, configEnvVar{key: k, value: val, force: force})
		}
	}
	return out
}
