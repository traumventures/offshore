package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The configuration is split in three:
//
//   - the client configuration (~/.config/cloudsourced/config.toml): where
//     the orchestrator runs ([control_plane]; absent = embedded), the user identity (user) and
//     local behaviour. Read by the CLI and the local daemon.
//   - the pool configuration ([pool], [providers.*]): machine sizes,
//     scheduling policy, lifetimes and providers. Read by the orchestrator
//     only: the control plane (its own file) or, in embedded mode, the
//     daemon. By default it is the client file, so a single file serves a
//     single-host setup.
//   - the repository configuration (.cloudsourced.toml, see Repo): per
//     repository settings that travel with each request.

// rawFile is the TOML schema shared by client and pool files, so that one
// file may hold both.
type rawFile struct {
	User         string        `toml:"user"`
	ControlPlane *ControlPlane `toml:"control_plane"`
	Pool         Pool          `toml:"pool"`
	Scheduler    Scheduler     `toml:"scheduler"`
	Machine      struct {
		Setup []ProvisionStep `toml:"setup"`
	} `toml:"machine"`
	Providers map[string]map[string]any `toml:"providers"`
	Storage   Storage                   `toml:"storage"`
	Telemetry Telemetry                 `toml:"telemetry"`
}

// Client is the client configuration.
type Client struct {
	// Path is the file the configuration was read from.
	Path string `toml:"-"`
	// User identifies the user towards the pool; it salts repository ids
	// (cache affinity, build slots). Default: $USER.
	User string `toml:"user"`
	// Orchestrator selects where pool decisions are made: in the local
	// daemon ("embedded") or in a shared control plane ("remote").
	ControlPlane ControlPlane `toml:"control_plane"`
	// Telemetry configures tracing of the proxy and the daemon.
	Telemetry Telemetry `toml:"telemetry"`
	// PoolSections lists pool sections present in the client file. They
	// configure the embedded orchestrator and have no effect in remote mode.
	PoolSections []string `toml:"-"`
	// Warnings are ineffective settings found while loading.
	Warnings []string `toml:"-"`
}

// ControlPlane configures the connection to a shared control plane.
type ControlPlane struct {
	// Mode is "remote" when [control_plane] is set, else "embedded" (the
	// daemon runs the orchestrator).
	Mode string `toml:"-"`
	// URL of a remote control plane, e.g. "https://cs.example.com" or
	// "https://10.0.0.5:7443".
	URL string `toml:"url"`
	// Fingerprint pins a self-signed control plane certificate (hex SHA-256,
	// printed by "cloudsourced controlplane"). Empty: verify against system CAs.
	Fingerprint string `toml:"fingerprint"`
	// APIKey is a reference (env:VAR, file:PATH, cmd:COMMAND) to the API key.
	APIKey string `toml:"api_key"`
	// ClientCert and ClientKey are PEM files of a client certificate for
	// control planes that require mutual TLS (--client-ca). Relative paths
	// are relative to the configuration file.
	ClientCert string `toml:"client_cert"`
	ClientKey  string `toml:"client_key"`
}

// ClientConfigPath returns the path of the client configuration file.
func ClientConfigPath() string {
	if p := os.Getenv("CLOUDSOURCED_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "cloudsourced", "config.toml")
}

// Embedded reports whether the orchestrator runs in the local daemon.
func (c *Client) Embedded() bool { return c.ControlPlane.Mode == "embedded" }

// Dir is the directory relative references and paths are resolved against.
func (c *Client) Dir() string { return filepath.Dir(c.Path) }

// LoadClient loads the client configuration, applying defaults when it is
// absent. Pool sections are accepted (embedded single-file setups) but not
// interpreted; use LoadPool for them.
func LoadClient() (*Client, error) { return LoadClientFrom(ClientConfigPath()) }

// LoadClientFrom loads the client configuration from path.
func LoadClientFrom(path string) (*Client, error) {
	c := &Client{Path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		var raw rawFile
		if err := decodeStrict(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		c.User, c.Telemetry = raw.User, raw.Telemetry
		if raw.ControlPlane != nil {
			c.ControlPlane = *raw.ControlPlane
			c.ControlPlane.Mode = "remote"
		}
		top, _ := topLevel(data)
		c.PoolSections = poolSections(top)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if c.User == "" {
		c.User = defaultUser()
	}
	o := &c.ControlPlane
	if o.Mode == "" {
		o.Mode = "embedded"
	}
	if o.Mode == "remote" && o.APIKey == "" {
		o.APIKey = "env:CLOUDSOURCED_API_KEY"
	}
	if len(c.PoolSections) > 0 {
		switch fp := PoolConfigPath(); {
		case o.Mode == "remote":
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %s have no effect in remote mode (the control plane's pool configuration applies)", path, strings.Join(c.PoolSections, ", ")))
		case fp != path:
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %s have no effect, the pool configuration is %s (CLOUDSOURCED_POOL_CONFIG)", path, strings.Join(c.PoolSections, ", "), fp))
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks the client configuration.
func (c *Client) Validate() error {
	var errs []string
	if strings.ContainsAny(c.User, "\x00\n") || strings.TrimSpace(c.User) == "" {
		errs = append(errs, fmt.Sprintf("user: invalid %q", c.User))
	}
	switch o := c.ControlPlane; o.Mode {
	case "embedded":
	case "remote":
		if !strings.HasPrefix(o.URL, "https://") && !strings.HasPrefix(o.URL, "http://") {
			errs = append(errs, "control_plane.url: an http(s) URL is required")
		}
		if o.Fingerprint != "" && !strings.HasPrefix(o.URL, "https://") {
			errs = append(errs, "control_plane.fingerprint: requires an https URL")
		}
		if (o.ClientCert == "") != (o.ClientKey == "") {
			errs = append(errs, "control_plane.client_cert and client_key must be set together")
		}
		if o.ClientCert != "" && !strings.HasPrefix(o.URL, "https://") {
			errs = append(errs, "control_plane.client_cert: requires an https URL")
		}
	default:
		errs = append(errs, fmt.Sprintf("orchestrator.mode: unsupported %q (embedded, remote)", o.Mode))
	}
	errs = append(errs, c.Telemetry.validate()...)
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s:\n  %s", c.Path, strings.Join(errs, "\n  "))
	}
	return nil
}

func defaultUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "default"
}

// validID accepts pool ids usable in labels and resource names.
func validID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (u *PoolConfig) displayPath() string {
	if u.Path == "" {
		return "pool configuration"
	}
	return u.Path
}

// topLevel returns the top level keys of a TOML document.
func topLevel(data []byte) (map[string]bool, error) {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out, nil
}

func sections(top map[string]bool, names ...string) []string {
	var out []string
	for _, n := range names {
		if top[n] {
			if n == "user" {
				out = append(out, n)
			} else {
				out = append(out, "["+n+"]")
			}
		}
	}
	sort.Strings(out)
	return out
}

func clientSections(top map[string]bool) []string {
	return sections(top, "control_plane", "user")
}
func poolSections(top map[string]bool) []string {
	return sections(top, "pool", "scheduler", "machine", "providers", "storage")
}
