// Package config parses the repository configuration (.cloudsourced.toml) and
// the user configuration (~/.config/cloudsourced/config.toml).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// RepoFileName is the name of the repository configuration file.
const RepoFileName = ".cloudsourced.toml"

// LocalFileName is an optional, not checked in, per-machine override.
const LocalFileName = ".cloudsourced.local.toml"

// SchemaVersion is the supported configuration schema version.
const SchemaVersion = 1

// Repo is the repository configuration. It describes what a remote
// environment needs, not where it runs.
type Repo struct {
	Version     int                `toml:"version"`
	Resources   Resources          `toml:"resources"`
	Environment Environment        `toml:"environment"`
	Source      Source             `toml:"source"`
	Cache       Cache              `toml:"cache"`
	Tools       map[string]Tool    `toml:"tool"`
	Secrets     map[string]string  `toml:"secrets"`
	Services    map[string]Service `toml:"services"`
	Outputs     Outputs            `toml:"outputs"`
	Editor      Editor             `toml:"editor"`

	// Root is the absolute directory containing the configuration file.
	Root string `toml:"-"`
}

// Resources is the per-session resource request used for scheduling.
// CPU and Memory are the initial estimate; with the history estimator the
// scheduler replaces them with measured values. MaxMemory is a hard limit.
type Resources struct {
	CPU       float64 `toml:"cpu"`
	Memory    string  `toml:"memory"`
	MaxMemory string  `toml:"max_memory"`
	Arch      string  `toml:"arch"`
}

// Environment describes how the remote environment image is built.
type Environment struct {
	Base       string            `toml:"base"`       // "image" | "dockerfile"
	Image      string            `toml:"image"`      // base image for base = "image"
	Dockerfile string            `toml:"dockerfile"` // path relative to the repo root
	Packages   Packages          `toml:"packages"`
	Toolchains Toolchains        `toml:"toolchains"`
	Setup      []SetupStep       `toml:"setup"`
	Env        map[string]string `toml:"env"`
	// Linker selects the linker for Rust builds: "" (toolchain default),
	// "mold" or "lld". The JSON tag keeps the environment hash of
	// repositories without a linker setting unchanged.
	Linker string `toml:"linker" json:"Linker,omitempty"`
}

// Linkers supported by environment.linker.
const (
	LinkerMold = "mold"
	LinkerLLD  = "lld"
)

// LinkerDriver is the path of the C compiler driver wrapper that the
// environment image installs for a linker (cc -fuse-ld=<linker>). Sessions
// use it as cargo's target linker.
func LinkerDriver(linker string) string { return "/usr/local/bin/cloudsourced-cc-" + linker }

// Packages lists system packages installed into the environment image.
type Packages struct {
	Apt []string `toml:"apt"`
}

// Toolchains references toolchain version files.
type Toolchains struct {
	Rust string `toml:"rust"` // path to rust-toolchain(.toml), relative to the repo root
}

// SetupStep is an environment provisioning command. Its result is cached by
// the hash of the previous layers, the command and the content of Inputs.
type SetupStep struct {
	Run    string   `toml:"run"`
	Inputs []string `toml:"inputs"`
}

// Source configures how local files are projected onto workers.
type Source struct {
	Default          string      `toml:"default"` // "watch"
	RespectGitignore *bool       `toml:"respect_gitignore"`
	Rules            []Rule      `toml:"rule"`
	ExtraRoots       []ExtraRoot `toml:"extra_root"`
}

// Rule assigns a projection mode to paths matching glob patterns.
type Rule struct {
	Paths []string `toml:"paths"`
	Mode  string   `toml:"mode"` // watch | immutable | hidden
}

// ExtraRoot projects an additional directory outside the repository.
type ExtraRoot struct {
	Path string `toml:"path"`
	Mode string `toml:"mode"`
}

// Cache configures worker side caches.
type Cache struct {
	Build    CacheClass    `toml:"build"`
	Deps     CacheClass    `toml:"deps"`
	Compiler CompilerCache `toml:"compiler"`
}

// CompilerCache configures a shared compiler cache ([cache.compiler]).
type CompilerCache struct {
	Tool string `toml:"tool"` // "" (off) or "sccache"
	// TTL lowers the maximum age of reused cache entries (at most 24h, the
	// cache's own limit).
	TTL string `toml:"ttl"`
	// Incremental = false sets CARGO_INCREMENTAL=0 in sessions, so that
	// workspace crates are cacheable too (sccache cannot cache incremental
	// compilations). Unset leaves cargo's default.
	Incremental *bool `toml:"incremental"`
}

// CompilerCacheSccache is the only supported compiler cache tool.
const CompilerCacheSccache = "sccache"

// MaxCompilerCacheTTL bounds cache.compiler.ttl.
const MaxCompilerCacheTTL = 24 * time.Hour

// CacheClass is a class of cached paths with a time to live.
type CacheClass struct {
	Paths []string `toml:"paths"`
	TTL   string   `toml:"ttl"`
}

// Tool is the profile of a proxied tool.
type Tool struct {
	Remote []string `toml:"remote"` // subcommands executed remotely; empty means all not listed in Local
	Local  []string `toml:"local"`  // subcommands always executed locally
	Eager  []string `toml:"eager"`  // output globs copied back after every run
	Env    []string `toml:"env"`    // environment variable allowlist (supports trailing *)
	// Ports are forwarded from 127.0.0.1 locally into the session while
	// it runs: 8080 or "local:remote" (e.g. "3000:3001").
	Ports []any `toml:"ports"`
}

// Service is an auxiliary container started next to every machine of the
// environment. It shares the machine's network namespace, so sessions reach
// it on localhost:Port.
type Service struct {
	Image   string            `toml:"image"`
	Env     map[string]string `toml:"env"`
	Port    int               `toml:"port"`
	Command []string          `toml:"command"`
}

// Outputs configures how remote build outputs appear locally.
type Outputs struct {
	// Mode "eager" (default) copies only the tools' eager outputs back
	// after every run. "fuse" additionally mounts the build directories
	// ([cache.build] paths) read-only at their local path, with content
	// fetched from the machine of the latest build on first access.
	Mode string `toml:"mode"`
}

// Output modes.
const (
	OutputsEager = "eager"
	OutputsFuse  = "fuse"
)

// Editor configures editor integration.
type Editor struct {
	// RustAnalyzer is "local" (default: rust-analyzer runs locally, checks
	// remotely, proc macros from eager outputs) or "lsp-remote" (checks and
	// build scripts remotely, proc macros and build script outputs from
	// the output FUSE).
	RustAnalyzer string `toml:"rust_analyzer"`
}

// rust-analyzer modes.
const (
	RustAnalyzerLocal     = "local"
	RustAnalyzerLSPRemote = "lsp-remote"
)

// Valid projection modes.
const (
	ModeWatch     = "watch"
	ModeImmutable = "immutable"
	ModeHidden    = "hidden"
)

// FindRepo walks up from dir to find the repository configuration.
func FindRepo(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, RepoFileName)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// LoadRepo loads and validates the repository configuration in root,
// applying the optional local override file.
func LoadRepo(root string) (*Repo, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var r Repo
	data, err := os.ReadFile(filepath.Join(root, RepoFileName))
	if err != nil {
		return nil, err
	}
	if err := decodeStrict(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", RepoFileName, err)
	}
	if local, err := os.ReadFile(filepath.Join(root, LocalFileName)); err == nil {
		if err := decodeStrict(local, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", LocalFileName, err)
		}
	}
	r.Root = root
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func decodeStrict(data []byte, v any) error {
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return errors.New(sm.String())
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return fmt.Errorf("line %d, column %d: %s", row, col, de.Error())
		}
		return err
	}
	return nil
}

func (r *Repo) applyDefaults() {
	if r.Version == 0 {
		r.Version = SchemaVersion
	}
	if r.Resources.CPU == 0 {
		r.Resources.CPU = 2
	}
	if r.Resources.Memory == "" {
		r.Resources.Memory = "4G"
	}
	if r.Resources.Arch == "" {
		r.Resources.Arch = "x86_64"
	}
	if r.Environment.Base == "" {
		if r.Environment.Dockerfile != "" {
			r.Environment.Base = "dockerfile"
		} else {
			r.Environment.Base = "image"
		}
	}
	if r.Source.Default == "" {
		r.Source.Default = ModeWatch
	}
	if r.Source.RespectGitignore == nil {
		t := true
		r.Source.RespectGitignore = &t
	}
	if r.Cache.Build.TTL == "" {
		r.Cache.Build.TTL = "24h"
	}
	if r.Cache.Deps.TTL == "" {
		r.Cache.Deps.TTL = "7d"
	}
	if r.Tools == nil {
		r.Tools = map[string]Tool{}
	}
	if r.Outputs.Mode == "" {
		r.Outputs.Mode = OutputsEager
	}
	if r.Editor.RustAnalyzer == "" {
		r.Editor.RustAnalyzer = RustAnalyzerLocal
	}
}

// Validate checks the configuration for semantic errors.
func (r *Repo) Validate() error {
	var errs []string
	if r.Version != SchemaVersion {
		errs = append(errs, fmt.Sprintf("version: unsupported schema version %d (want %d)", r.Version, SchemaVersion))
	}
	if r.Resources.CPU < 0 {
		errs = append(errs, "resources.cpu: must be positive")
	}
	if _, err := ParseBytes(r.Resources.Memory); err != nil {
		errs = append(errs, "resources.memory: "+err.Error())
	}
	if r.Resources.MaxMemory != "" {
		if _, err := ParseBytes(r.Resources.MaxMemory); err != nil {
			errs = append(errs, "resources.max_memory: "+err.Error())
		}
	}
	switch r.Resources.Arch {
	case "x86_64", "aarch64":
	default:
		errs = append(errs, fmt.Sprintf("resources.arch: unsupported %q", r.Resources.Arch))
	}
	switch r.Environment.Base {
	case "image":
		if r.Environment.Image == "" {
			errs = append(errs, "environment.image: required when environment.base = \"image\"")
		}
	case "dockerfile":
		if r.Environment.Dockerfile == "" {
			errs = append(errs, "environment.dockerfile: required when environment.base = \"dockerfile\"")
		}
	default:
		errs = append(errs, fmt.Sprintf("environment.base: unsupported %q (image, dockerfile)", r.Environment.Base))
	}
	for i, s := range r.Environment.Setup {
		if strings.TrimSpace(s.Run) == "" {
			errs = append(errs, fmt.Sprintf("environment.setup[%d].run: required", i))
		}
		for _, in := range s.Inputs {
			if filepath.IsAbs(in) || strings.HasPrefix(filepath.Clean(in), "..") {
				errs = append(errs, fmt.Sprintf("environment.setup[%d].inputs: %q must be inside the repository", i, in))
			}
		}
	}
	if r.Source.Default != ModeWatch && r.Source.Default != ModeImmutable {
		errs = append(errs, fmt.Sprintf("source.default: unsupported %q", r.Source.Default))
	}
	for i, rule := range r.Source.Rules {
		switch rule.Mode {
		case ModeWatch, ModeImmutable, ModeHidden:
		default:
			errs = append(errs, fmt.Sprintf("source.rule[%d].mode: unsupported %q", i, rule.Mode))
		}
		if len(rule.Paths) == 0 {
			errs = append(errs, fmt.Sprintf("source.rule[%d].paths: required", i))
		}
	}
	for i, er := range r.Source.ExtraRoots {
		if er.Path == "" {
			errs = append(errs, fmt.Sprintf("source.extra_root[%d].path: required", i))
		}
		if er.Mode != "" && er.Mode != ModeWatch && er.Mode != ModeImmutable {
			errs = append(errs, fmt.Sprintf("source.extra_root[%d].mode: unsupported %q", i, er.Mode))
		}
	}
	for _, p := range r.Cache.Build.Paths {
		if filepath.IsAbs(p) || strings.HasPrefix(filepath.Clean(p), "..") || strings.ContainsAny(p, "*?[") {
			errs = append(errs, fmt.Sprintf("cache.build.paths: %q must be a plain directory inside the repository", p))
		}
	}
	if _, err := ParseDuration(r.Cache.Build.TTL); err != nil {
		errs = append(errs, "cache.build.ttl: "+err.Error())
	}
	if _, err := ParseDuration(r.Cache.Deps.TTL); err != nil {
		errs = append(errs, "cache.deps.ttl: "+err.Error())
	}
	if r.Outputs.Mode != OutputsEager && r.Outputs.Mode != OutputsFuse {
		errs = append(errs, fmt.Sprintf("outputs.mode: unsupported %q (eager, fuse)", r.Outputs.Mode))
	}
	if r.Outputs.Mode == OutputsFuse && len(r.Cache.Build.Paths) == 0 {
		errs = append(errs, "outputs.mode: \"fuse\" needs [cache.build] paths")
	}
	if r.Editor.RustAnalyzer != RustAnalyzerLocal && r.Editor.RustAnalyzer != RustAnalyzerLSPRemote {
		errs = append(errs, fmt.Sprintf("editor.rust_analyzer: unsupported %q (local, lsp-remote)", r.Editor.RustAnalyzer))
	}
	switch r.Environment.Linker {
	case "", LinkerMold, LinkerLLD:
	default:
		errs = append(errs, fmt.Sprintf("environment.linker: unsupported %q (mold, lld)", r.Environment.Linker))
	}
	cc := r.Cache.Compiler
	switch cc.Tool {
	case "":
		if cc.TTL != "" || cc.Incremental != nil {
			errs = append(errs, "cache.compiler: ttl and incremental require tool = \"sccache\"")
		}
	case CompilerCacheSccache:
	default:
		errs = append(errs, fmt.Sprintf("cache.compiler.tool: unsupported %q (sccache)", cc.Tool))
	}
	if cc.TTL != "" {
		if d, err := ParseDuration(cc.TTL); err != nil {
			errs = append(errs, "cache.compiler.ttl: "+err.Error())
		} else if d < time.Second || d > MaxCompilerCacheTTL {
			errs = append(errs, fmt.Sprintf("cache.compiler.ttl: %q must be between 1s and 24h", cc.TTL))
		}
	}
	for name, t := range r.Tools {
		if _, err := ParsePorts(t.Ports); err != nil {
			errs = append(errs, fmt.Sprintf("tool.%s.ports: %v", name, err))
		}
	}
	for name, svc := range r.Services {
		if !validName(name) {
			errs = append(errs, fmt.Sprintf("services.%s: name must match [a-z0-9-]+", name))
		}
		if svc.Image == "" {
			errs = append(errs, fmt.Sprintf("services.%s.image: required", name))
		}
	}
	for name, ref := range r.Secrets {
		kind, _, ok := strings.Cut(ref, ":")
		if !ok || (kind != "env" && kind != "file" && kind != "cmd") {
			errs = append(errs, fmt.Sprintf("secrets.%s: must be \"env:VAR\", \"file:PATH\" or \"cmd:COMMAND\"", name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s:\n  %s", filepath.Join(r.Root, RepoFileName), strings.Join(errs, "\n  "))
	}
	return nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ExtraRootPaths returns the absolute paths of extra roots.
func (r *Repo) ExtraRootPaths() []string {
	var out []string
	for _, er := range r.Source.ExtraRoots {
		p := er.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.Root, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// CacheTTLs returns cache.build.ttl and cache.deps.ttl. They are repository
// settings that travel with every request; the fleet caps them by its
// max_lifetime.
func (r *Repo) CacheTTLs() (build, deps time.Duration) {
	build, _ = ParseDuration(r.Cache.Build.TTL)
	deps, _ = ParseDuration(r.Cache.Deps.TTL)
	return build, deps
}

// CompilerCacheTTL returns cache.compiler.ttl (0 = not set).
func (r *Repo) CompilerCacheTTL() time.Duration {
	if r.Cache.Compiler.TTL == "" {
		return 0
	}
	d, _ := ParseDuration(r.Cache.Compiler.TTL)
	return d
}

// MemoryBytes returns the requested session memory in bytes.
func (r *Repo) MemoryBytes() int64 {
	b, _ := ParseBytes(r.Resources.Memory)
	return b
}

// MaxMemoryBytes returns the hard memory limit (0 = none).
func (r *Repo) MaxMemoryBytes() int64 {
	if r.Resources.MaxMemory == "" {
		return 0
	}
	b, _ := ParseBytes(r.Resources.MaxMemory)
	return b
}

// ParseDuration parses Go durations plus a "d" (days) suffix.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// ParseBytes parses sizes like "512M", "24G", "1T" or plain bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			s = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}
