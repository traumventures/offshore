package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/traumventures/cloudsourced/internal/config"
)

const configUsage = `usage:
  cloudsourced config check [--pool FILE]   validate the configuration files of this mode
  cloudsourced config show  [--pool FILE]   print the effective configuration (secrets redacted)

Without --pool both commands work on the files this client uses: the client
configuration, the pool configuration when the orchestrator is embedded,
and the repository configuration of the current directory. --pool FILE
checks or shows only a pool configuration, as "cloudsourced controlplane
--config FILE" reads it.
`

// configCmd implements "cloudsourced config".
func configCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, configUsage)
		return errors.New("config: missing subcommand (check, show)")
	}
	sub := args[0]
	fs := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), configUsage) }
	poolPath := fs.String("pool", "", "only this pool configuration (control plane role)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("config %s: unexpected argument %q", sub, fs.Arg(0))
	}
	set, err := loadConfigSet(*poolPath)
	if err != nil {
		return err
	}
	switch sub {
	case "check":
		set.check(out)
		return nil
	case "show":
		return set.show(out)
	}
	fmt.Fprint(out, configUsage)
	return fmt.Errorf("config: unknown subcommand %q (check, show)", sub)
}

// configSet are the configuration files in effect, each with its role.
type configSet struct {
	client   *config.Client     // nil with --pool
	pool     *config.PoolConfig // nil in remote mode
	poolRole string
	repo     *config.Repo
}

func loadConfigSet(poolPath string) (*configSet, error) {
	s := &configSet{}
	if poolPath != "" {
		f, err := config.LoadPool(poolPath)
		if err != nil {
			return nil, err
		}
		s.pool, s.poolRole = f, "control plane (cloudsourced controlplane --config)"
		return s, nil
	}
	c, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	s.client = c
	if c.Embedded() {
		f, err := config.LoadPool(config.PoolConfigPath())
		if err != nil {
			return nil, err
		}
		s.pool, s.poolRole = f, "embedded orchestrator (local daemon)"
	}
	if root, err := config.FindRepo("."); err == nil {
		r, err := config.LoadRepo(root)
		if err != nil {
			return nil, err
		}
		s.repo = r
	}
	return s, nil
}

func describe(path string) string {
	if path == "" {
		return "(defaults)"
	}
	return path
}

func fileState(path string) string {
	if _, err := os.Stat(path); err != nil {
		return " (absent, defaults)"
	}
	return ""
}

func (s *configSet) check(out io.Writer) {
	if c := s.client; c != nil {
		fmt.Fprintf(out, "client configuration ok: %s%s\n  role: client (user %q, orchestrator %s", c.Path, fileState(c.Path), c.User, c.ControlPlane.Mode)
		if !c.Embedded() {
			fmt.Fprintf(out, " at %s", c.ControlPlane.URL)
		}
		fmt.Fprintln(out, ")")
		for _, w := range c.Warnings {
			fmt.Fprintln(out, "  warning:", w)
		}
		if !c.Embedded() {
			fmt.Fprintln(out, "pool configuration: not used by this client (the control plane's applies)")
		}
	}
	if f := s.pool; f != nil {
		fmt.Fprintf(out, "pool configuration ok: %s%s\n  role: %s, pool %q, providers %s\n",
			describe(f.Path), fileState(f.Path), s.poolRole, f.Pool.ID, strings.Join(f.ProviderNames(), ", "))
		for _, w := range f.Warnings {
			if s.client != nil && strings.Contains(w, "ignored by the pool (client settings)") && f.Path == s.client.Path {
				continue // single-file embedded setup: the client part is expected
			}
			fmt.Fprintln(out, "  warning:", w)
		}
	}
	if r := s.repo; r != nil {
		fmt.Fprintf(out, "repository configuration ok: %s\n  role: repository (sent with every request)\n", filepath.Join(r.Root, config.RepoFileName))
	}
}

func (s *configSet) show(out io.Writer) error {
	section := func(title string, v any, drop ...string) error {
		m, ok := v.(map[string]any)
		if !ok {
			var err error
			if m, err = toMap(v); err != nil {
				return err
			}
		}
		for _, k := range drop {
			deletePath(m, k)
		}
		redact(m, "")
		b, err := toml.Marshal(m)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "# %s\n%s\n", title, bytes.TrimSpace(b))
		fmt.Fprintln(out)
		return nil
	}
	if c := s.client; c != nil {
		var drop []string
		if c.Embedded() {
			drop = append(drop, "control_plane")
		}
		if err := section("client configuration: "+c.Path, c, drop...); err != nil {
			return err
		}
	}
	if f := s.pool; f != nil {
		doc, err := f.Document()
		if err != nil {
			return err
		}
		if err := section("pool configuration: "+describe(f.Path)+" ("+s.poolRole+")", doc); err != nil {
			return err
		}
	}
	if r := s.repo; r != nil {
		if err := section("repository configuration: "+filepath.Join(r.Root, config.RepoFileName), r); err != nil {
			return err
		}
	}
	return nil
}

func toMap(v any) (map[string]any, error) {
	b, err := toml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, toml.Unmarshal(b, &m)
}

func deletePath(m map[string]any, path string) {
	head, rest, ok := strings.Cut(path, ".")
	if !ok {
		delete(m, path)
		return
	}
	if sub, ok := m[head].(map[string]any); ok {
		deletePath(sub, rest)
	}
}

// secretKey reports whether a key may hold a secret value.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"secret", "password", "token", "api_key", "apikey", "access_key", "private", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redact replaces literal secret values. References (env:, file:, cmd:)
// name where a secret comes from and are kept.
func redact(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = redact(e, k)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = redact(e, key)
		}
		return t
	case string:
		if !secretKey(key) || t == "" {
			return t
		}
		for _, p := range []string{"env:", "file:", "cmd:"} {
			if strings.HasPrefix(t, p) {
				return t
			}
		}
		return "<redacted>"
	}
	return v
}
