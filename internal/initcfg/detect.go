// Package initcfg generates a repository configuration (.offshore.toml)
// from what a repository already declares: its Cargo workspace and Rust
// toolchain file, the system packages, services and setup steps of its
// GitHub Actions workflows (including local composite actions) or
// Dockerfiles, and its git dependencies. Every detected item carries a short
// explanation that Render writes as a comment next to it.
package initcfg

import (
	"bufio"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/traumventures/offshore/internal/adapter"
	"go.yaml.in/yaml/v3"
)

// Detection is what Detect found in a repository.
type Detection struct {
	Root string
	// Workspace is the Cargo workspace directory relative to Root ("" =
	// root); Rust is false when the repository has no Cargo project.
	Workspace string
	Rust      bool
	Toolchain string // rust-toolchain(.toml), relative to Root
	// ToolchainNote describes the pinned channel, components and targets.
	ToolchainNote string

	CPU      int
	CPUNote  string
	Image    string
	ImageWhy string
	Apt      []string
	AptWhy   string
	Linker   string
	Setup    []Step
	Env      []KV
	Services []Service
	Secrets  []KV
	GitDeps  []string // git dependency URLs found in Cargo manifests

	// Adapters are the detected ecosystems (internal/adapter names);
	// Caches merges their default caches.
	Adapters []string
	Caches   []adapter.Cache
	// Tools are [tool.X] sections besides cargo's.
	Tools []ToolSection

	CargoLocal []string
	CargoEnv   []string
	EnvWhy     string

	// Notes are detected items that were deliberately not adopted.
	Notes []string
}

// ToolSection is a [tool.X] section of a detected ecosystem.
type ToolSection struct {
	Name    string
	Comment string
	Env     []string
	Local   []string
}

// Step is an environment setup step.
type Step struct {
	Comment string
	Run     string
}

// KV is a key with a value and the reason it was detected.
type KV struct{ Key, Value, Why string }

// Service is an auxiliary container.
type Service struct {
	Name    string
	Image   string
	Env     []KV
	Port    int
	Command []string
	Why     string
}

// ciOnlyPackages are installed by CI jobs for the runner itself (sticky
// disks, checkouts, privilege changes), not for the build.
var ciOnlyPackages = map[string]string{
	"sudo": "", "e2fsprogs": "", "unzip": "", "openssh-client": "", "gnupg": "", "lsb-release": "",
	"software-properties-common": "", "apt-transport-https": "", "jq": "", "rsync": "", "zstd": "",
}

// ciOnlyEnv are environment variables that configure the CI runner or its
// caches and must not be copied into the remote environment.
var ciOnlyEnv = []string{"CARGO_TERM_", "CARGO_TARGET_DIR", "CARGO_INCREMENTAL", "CARGO_HOME", "RUSTUP_HOME",
	"STICKY", "GITHUB_", "RUNNER_", "CI", "SCCACHE_", "RUSTC_WRAPPER", "ACTIONS_"}

// knownTools are setup recipes for tools CI installs through actions.
var knownTools = map[string]Step{
	"cargo-nextest": {Comment: "cargo nextest", Run: "curl -fsSL https://get.nexte.st/latest/linux | tar -xzf - -C /usr/local/bin && cargo nextest --version"},
}

// CargoLocal are cargo subcommands that only touch the local checkout or
// the user's account and therefore always run locally.
var CargoLocal = []string{"fmt", "publish", "login", "install", "new", "init", "add", "remove", "update"}

// credentialHelperStep makes git (and cargo with CARGO_NET_GIT_FETCH_WITH_CLI)
// authenticate to github.com with the GH_TOKEN secret of the session.
const credentialHelperStep = `printf '#!/bin/sh\n[ "$1" = get ] && printf "username=x-access-token\\npassword=%%s\\n" "$GH_TOKEN"\n' > /usr/local/bin/git-credential-ghtoken \
 && chmod 755 /usr/local/bin/git-credential-ghtoken \
 && git config --system credential.https://github.com.helper /usr/local/bin/git-credential-ghtoken`

// Detect inspects the repository at root. It never modifies it.
func Detect(root string) (*Detection, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	d := &Detection{Root: root, CPU: 4, CPUNote: "default request; the scheduler learns the real need per command"}
	d.Workspace, d.Rust = findWorkspace(root)
	if d.Rust {
		d.detectToolchain()
		d.Linker = "mold"
		d.CargoLocal = CargoLocal
		d.CargoEnv = []string{"RUSTFLAGS", "CARGO_TERM_*", "RUST_LOG", "RUST_BACKTRACE"}
	}
	d.detectAdapters()
	ci := d.scanWorkflows()
	if !ci.found {
		d.scanDockerfiles(ci)
	}
	d.applyCI(ci)
	if d.Rust {
		d.detectGitDeps()
		d.detectTestTools(ci)
	}
	d.finishPackages()
	return d, nil
}

// findWorkspace returns the directory of the Cargo workspace: the root's
// Cargo.toml, else the shallowest [workspace] manifest below it.
func findWorkspace(root string) (string, bool) {
	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		return "", true
	}
	wsRe := regexp.MustCompile(`(?m)^\[workspace\]`)
	type cand struct {
		dir   string
		depth int
		ws    bool
	}
	var best *cand
	filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if de.IsDir() {
			if p != root && (skipDir(de.Name()) || strings.Count(rel, "/") >= 3) {
				return filepath.SkipDir
			}
			return nil
		}
		if de.Name() != "Cargo.toml" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		c := cand{dir: dir, depth: strings.Count(dir, "/"), ws: wsRe.Match(data)}
		if best == nil || (c.ws && !best.ws) || (c.ws == best.ws && c.depth < best.depth) {
			best = &c
		}
		return nil
	})
	if best == nil {
		return "", false
	}
	return best.dir, true
}

func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "target" || name == "node_modules" || name == "vendor" || name == "dist"
}

func (d *Detection) detectToolchain() {
	for _, dir := range []string{d.Workspace, ""} {
		for _, name := range []string{"rust-toolchain.toml", "rust-toolchain"} {
			rel := filepath.ToSlash(filepath.Join(dir, name))
			data, err := os.ReadFile(filepath.Join(d.Root, rel))
			if err != nil {
				continue
			}
			d.Toolchain = rel
			var tc struct {
				Toolchain struct {
					Channel    string   `toml:"channel"`
					Components []string `toml:"components"`
					Targets    []string `toml:"targets"`
				} `toml:"toolchain"`
			}
			var parts []string
			if toml.Unmarshal(data, &tc) == nil && tc.Toolchain.Channel != "" {
				parts = append(parts, "channel "+tc.Toolchain.Channel)
				if len(tc.Toolchain.Components) > 0 {
					parts = append(parts, "components "+strings.Join(tc.Toolchain.Components, ", "))
				}
				if len(tc.Toolchain.Targets) > 0 {
					parts = append(parts, "targets "+strings.Join(tc.Toolchain.Targets, ", "))
				}
			} else if ch := strings.TrimSpace(string(data)); ch != "" && !strings.Contains(ch, "\n") {
				parts = append(parts, "channel "+ch)
			}
			d.ToolchainNote = "rustup installs what the file pins"
			if len(parts) > 0 {
				d.ToolchainNote += ": " + strings.Join(parts, "; ")
			}
			return
		}
	}
}

// --- CI workflows ---------------------------------------------------------

// ciFacts collects what the Rust jobs of the CI declare.
type ciFacts struct {
	found     bool
	image     string
	imageFrom string
	runsOn    string
	apt       []string
	aptFrom   []string
	setup     []Step
	tools     map[string]string // tool -> where it was installed
	ignored   []string
	dropped   []string
	env       []KV
	stepEnv   []string
	services  []Service
	postgres  string // version found in package names
	text      strings.Builder
}

type ghStep struct {
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
}

type ghJob struct {
	RunsOn    any                  `yaml:"runs-on"`
	Container any                  `yaml:"container"`
	Services  map[string]ghService `yaml:"services"`
	Env       map[string]string    `yaml:"env"`
	Defaults  struct {
		Run struct {
			WorkingDirectory string `yaml:"working-directory"`
		} `yaml:"run"`
	} `yaml:"defaults"`
	Steps []ghStep `yaml:"steps"`
}

type ghService struct {
	Image   string            `yaml:"image"`
	Env     map[string]string `yaml:"env"`
	Ports   []any             `yaml:"ports"`
	Options string            `yaml:"options"`
}

type ghWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ghJob  `yaml:"jobs"`
}

type ghAction struct {
	Runs struct {
		Using string   `yaml:"using"`
		Steps []ghStep `yaml:"steps"`
	} `yaml:"runs"`
}

func (d *Detection) scanWorkflows() *ciFacts {
	ci := &ciFacts{tools: map[string]string{}}
	files, _ := filepath.Glob(filepath.Join(d.Root, ".github", "workflows", "*.y*ml"))
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var wf ghWorkflow
		if yaml.Unmarshal(data, &wf) != nil {
			continue
		}
		names := make([]string, 0, len(wf.Jobs))
		for n := range wf.Jobs {
			names = append(names, n)
		}
		sort.Strings(names)
		rel, _ := filepath.Rel(d.Root, f)
		wfRelevant := false
		for _, n := range names {
			job := wf.Jobs[n]
			steps := d.expandSteps(job.Steps, 0)
			if !d.rustJob(job, steps) {
				continue
			}
			wfRelevant, ci.found = true, true
			d.collectJob(ci, filepath.ToSlash(rel)+" job "+n, job, steps)
			ci.text.Write(data)
		}
		if wfRelevant {
			for _, k := range sortedKeys(wf.Env) {
				addEnv(ci, k, wf.Env[k], filepath.ToSlash(rel))
			}
		}
	}
	return ci
}

// expandSteps inlines the steps of local composite actions (uses: ./path).
func (d *Detection) expandSteps(steps []ghStep, depth int) []ghStep {
	var out []ghStep
	for _, s := range steps {
		if strings.HasPrefix(s.Uses, "./") && depth < 4 {
			if a, ok := d.loadAction(s.Uses); ok {
				out = append(out, d.expandSteps(a.Runs.Steps, depth+1)...)
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func (d *Detection) loadAction(uses string) (*ghAction, bool) {
	dir := filepath.Join(d.Root, filepath.FromSlash(strings.TrimPrefix(uses, "./")))
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var a ghAction
		if yaml.Unmarshal(data, &a) != nil || a.Runs.Using != "composite" {
			return nil, false
		}
		return &a, true
	}
	return nil, false
}

// rustJob reports whether a job builds the Cargo workspace.
func (d *Detection) rustJob(job ghJob, steps []ghStep) bool {
	if !d.Rust {
		return false
	}
	if d.Workspace != "" && cleanDir(job.Defaults.Run.WorkingDirectory) == d.Workspace {
		return true
	}
	for _, s := range steps {
		if regexp.MustCompile(`(^|[\s;&|(])cargo\s`).MatchString(s.Run) ||
			strings.Contains(s.Uses, "rust-toolchain") || strings.HasPrefix(s.Uses, "actions-rs/") {
			return true
		}
	}
	return false
}

func cleanDir(p string) string {
	p = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
	if p == "." {
		return ""
	}
	return p
}

func (d *Detection) collectJob(ci *ciFacts, where string, job ghJob, steps []ghStep) {
	if img := containerImage(job.Container); img != "" && ci.image == "" {
		ci.image, ci.imageFrom = img, where
	}
	if ro, ok := job.RunsOn.(string); ok && ci.runsOn == "" {
		ci.runsOn = ro
	}
	for _, k := range sortedKeys(job.Env) {
		if v := job.Env[k]; strings.HasPrefix(v, "/") && !hasExpr(v) && !ciOnly(k) {
			ci.dropped = append(ci.dropped, fmt.Sprintf("env %s=%s (%s) not added: a path on the CI runner", k, v, where))
			continue
		}
		addEnv(ci, k, job.Env[k], where)
	}
	for _, name := range sortedKeys(job.Services) {
		s := job.Services[name]
		if s.Image == "" || hasExpr(s.Image) {
			continue
		}
		svc := Service{Name: serviceName(name), Image: s.Image, Why: "service container of " + where}
		for _, k := range sortedKeys(s.Env) {
			if !hasExpr(s.Env[k]) {
				svc.Env = append(svc.Env, KV{Key: k, Value: s.Env[k]})
			}
		}
		for _, p := range s.Ports {
			fmt.Sscanf(strings.Split(fmt.Sprint(p), ":")[0], "%d", &svc.Port)
			break
		}
		ci.addService(svc)
	}
	for _, s := range steps {
		for _, k := range sortedKeys(s.Env) {
			if !ciOnly(k) && !strings.Contains(s.Env[k], "secrets.") && !strings.Contains(s.Env[k], "inputs.") &&
				!strings.Contains(s.Env[k], "steps.") && !strings.Contains(s.Env[k], "github.") && !strings.Contains(s.Env[k], "runner.") {
				ci.stepEnv = append(ci.stepEnv, k)
			}
		}
		switch {
		case strings.HasPrefix(s.Uses, "taiki-e/install-action"):
			for _, t := range strings.Split(fmt.Sprint(s.With["tool"]), ",") {
				t = strings.TrimSpace(t)
				if t == "" || t == "<nil>" {
					continue
				}
				if _, ok := ci.tools[t]; !ok {
					ci.tools[t] = where
				}
			}
		case s.Uses != "" && !strings.HasPrefix(s.Uses, "actions/checkout") && !strings.Contains(s.Uses, "checkout@") &&
			!strings.Contains(s.Uses, "cache") && !strings.Contains(s.Uses, "stickydisk") && !strings.Contains(s.Uses, "upload-artifact") &&
			!strings.Contains(s.Uses, "rust-toolchain") && !strings.HasPrefix(s.Uses, "actions-rs/"):
			name := strings.SplitN(s.Uses, "@", 2)[0]
			if !slices.Contains(ci.ignored, name) {
				ci.ignored = append(ci.ignored, name)
			}
		}
		if s.Run != "" {
			ci.script(s.Run, where)
		}
	}
}

func containerImage(c any) string {
	switch v := c.(type) {
	case string:
		if !hasExpr(v) {
			return v
		}
	case map[string]any:
		if s, ok := v["image"].(string); ok && !hasExpr(s) {
			return s
		}
	}
	return ""
}

func hasExpr(s string) bool { return strings.Contains(s, "${{") }

func ciOnly(k string) bool {
	for _, p := range ciOnlyEnv {
		if k == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(k, p)) || (p == "STICKY" && strings.HasPrefix(k, p)) {
			return true
		}
	}
	return false
}

func addEnv(ci *ciFacts, k, v, where string) {
	if ciOnly(k) || hasExpr(v) {
		return
	}
	for _, e := range ci.env {
		if e.Key == k {
			return
		}
	}
	ci.env = append(ci.env, KV{Key: k, Value: v, Why: "from " + where})
}

func (ci *ciFacts) addService(s Service) {
	for _, e := range ci.services {
		if e.Name == s.Name {
			return
		}
	}
	ci.services = append(ci.services, s)
}

func serviceName(n string) string {
	n = strings.ToLower(n)
	var b strings.Builder
	for _, r := range n {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

var (
	postgresPkg = regexp.MustCompile(`\bpostgresql-(\d+)\b`)
	urlRe       = regexp.MustCompile(`https?://\S+`)
)

// logicalLines joins shell continuation lines and keeps comments.
func logicalLines(script string) []string {
	var out []string
	var cur strings.Builder
	sc := bufio.NewScanner(strings.NewReader(script))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasSuffix(line, "\\") {
			cur.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")) + " ")
			continue
		}
		cur.WriteString(line)
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// script extracts apt packages and download-and-install blocks from a run
// script (or a Dockerfile RUN instruction).
func (ci *ciFacts) script(run, where string) {
	ci.text.WriteString(run + "\n")
	if m := postgresPkg.FindStringSubmatch(run); m != nil && ci.postgres == "" {
		ci.postgres = m[1]
	}
	lines := logicalLines(run)
	var comment string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "#") {
			if comment == "" {
				comment = strings.TrimSpace(strings.TrimPrefix(l, "#"))
			}
			continue
		}
		if pk := aptPackages(l); len(pk) > 0 {
			ci.apt = appendUnique(ci.apt, pk...)
			if !slices.Contains(ci.aptFrom, where) {
				ci.aptFrom = append(ci.aptFrom, where)
			}
		}
		if file, ok := download(l); ok {
			block := []string{l}
			j := i + 1
			for ; j < len(lines); j++ {
				n := lines[j]
				if strings.HasPrefix(n, "#") {
					break
				}
				if (file != "" && strings.Contains(n, file)) || versionCheck(n) {
					block = append(block, n)
					if versionCheck(n) {
						j++
						break
					}
					continue
				}
				break
			}
			if file != "" && !slices.ContainsFunc(block, func(s string) bool { return strings.HasPrefix(s, "rm ") }) {
				last := len(block)
				if versionCheck(block[last-1]) {
					last--
				}
				block = slices.Insert(block, last, "rm -f "+file)
			}
			c := strings.TrimRight(comment, ",.;:")
			if c == "" {
				c = "from " + where
			}
			run := strings.Join(block, " \\\n && ")
			if !slices.ContainsFunc(ci.setup, func(s Step) bool { return s.Run == run }) {
				ci.setup = append(ci.setup, Step{Comment: c, Run: run})
			}
			i = j - 1
		}
		comment = ""
	}
}

// download recognises "curl/wget URL -o FILE" and "curl URL | tar/sh"
// lines; file is the downloaded path ("" for pipes).
func download(l string) (string, bool) {
	f := strings.Fields(l)
	if len(f) == 0 || (f[0] != "curl" && f[0] != "wget") || !urlRe.MatchString(l) {
		return "", false
	}
	for i, a := range f {
		if (a == "-o" || a == "-O" || a == "--output") && i+1 < len(f) {
			return f[i+1], true
		}
	}
	if strings.Contains(l, "| tar") || strings.Contains(l, "| sh") || strings.Contains(l, "| bash") {
		return "", true
	}
	return "", false
}

func versionCheck(l string) bool {
	f := strings.Fields(l)
	return len(f) >= 2 && len(f) <= 3 && (f[len(f)-1] == "--version" || f[len(f)-1] == "-V") && !strings.ContainsAny(l, "|&;")
}

// aptPackages returns the packages of an apt-get/apt install command.
func aptPackages(l string) []string {
	var out []string
	for _, part := range regexp.MustCompile(`&&|;|\|\|`).Split(l, -1) {
		f := strings.Fields(part)
		idx := -1
		for i := 0; i+1 < len(f); i++ {
			if (f[i] == "apt-get" || f[i] == "apt") && f[i+1] == "install" {
				idx = i + 2
				break
			}
		}
		if idx < 0 {
			continue
		}
		for _, a := range f[idx:] {
			if a == "|" || strings.HasPrefix(a, ">") || strings.HasPrefix(a, "2>") {
				break
			}
			if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "$\"'`(){}*") || strings.Contains(a, "=") && strings.HasPrefix(a, "DEBIAN") {
				continue
			}
			a, _, _ = strings.Cut(a, "/")
			a, _, _ = strings.Cut(a, "=")
			if regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`).MatchString(a) {
				out = append(out, a)
			}
		}
	}
	return out
}

func appendUnique(s []string, v ...string) []string {
	for _, x := range v {
		if !slices.Contains(s, x) {
			s = append(s, x)
		}
	}
	return s
}

// --- Dockerfiles ------------------------------------------------------------

// scanDockerfiles uses an obvious Rust build Dockerfile (FROM rust:...)
// when no CI workflow builds the workspace.
func (d *Detection) scanDockerfiles(ci *ciFacts) {
	if !d.Rust {
		return
	}
	var cands []string
	for _, dir := range []string{"", d.Workspace} {
		m, _ := filepath.Glob(filepath.Join(d.Root, dir, "*Dockerfile*"))
		cands = append(cands, m...)
	}
	for _, f := range cands {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(d.Root, f)
		var image string
		for _, l := range logicalLines(string(data)) {
			fields := strings.Fields(l)
			if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
				if strings.HasPrefix(fields[1], "rust:") && image == "" {
					image = fields[1]
				}
				continue
			}
			if len(fields) >= 2 && strings.EqualFold(fields[0], "RUN") && image != "" {
				ci.script(strings.TrimSpace(l[3:]), filepath.ToSlash(rel))
			}
		}
		if image != "" {
			ci.found, ci.image, ci.imageFrom = true, image, filepath.ToSlash(rel)
			return
		}
	}
}

// --- assembling -------------------------------------------------------------

func (d *Detection) applyCI(ci *ciFacts) {
	switch {
	case ci.image != "":
		d.Image, d.ImageWhy = baseImage(ci.image, d.Toolchain != ""), "CI container "+ci.image+" of "+ci.imageFrom
		if d.Image != ci.image {
			d.ImageWhy += "; slim variant, the toolchain file pins the Rust version"
		}
	case d.Rust:
		d.Image, d.ImageWhy = "rust:1-slim-trixie", "default Rust image (no CI container or Dockerfile found)"
	case d.has("go"):
		d.Image, d.ImageWhy = goImage(d.Root), "default Go image for the go directive of go.mod (no CI container or Dockerfile found)"
	case d.has("node"):
		d.Image, d.ImageWhy = nodeImage(d.Root), "default image of the package manager (no CI container or Dockerfile found)"
	default:
		d.Image, d.ImageWhy = "debian:trixie-slim", "default image (no Cargo workspace found)"
	}
	if m := regexp.MustCompile(`(\d+)-?vcpu|(\d+)-cores?`).FindStringSubmatch(ci.runsOn); m != nil {
		n := 0
		fmt.Sscanf(m[1]+m[2], "%d", &n)
		if n > 0 {
			d.CPU = max(2, int(math.Round(float64(n)*0.625)))
			d.CPUNote = fmt.Sprintf("CI runs on %s (%d vCPUs); sessions share machines, the scheduler learns the real need", ci.runsOn, n)
		}
	}
	for _, p := range ci.apt {
		switch {
		case postgresPkg.MatchString(p) || strings.HasPrefix(p, "postgresql"):
			// a service container instead, see below
		case func() bool { _, ok := ciOnlyPackages[p]; return ok }():
			d.Notes = append(d.Notes, "apt package "+p+" not added: used by the CI runner, not the build")
		default:
			d.Apt = append(d.Apt, p)
		}
	}
	if len(ci.aptFrom) > 0 {
		d.AptWhy = "from " + strings.Join(ci.aptFrom, ", ")
	}
	d.Setup = append(d.Setup, ci.setup...)
	d.Env = append(d.Env, ci.env...)
	d.Services = append(d.Services, ci.services...)
	if ci.postgres != "" && !slices.ContainsFunc(d.Services, func(s Service) bool { return strings.HasPrefix(s.Image, "postgres") }) {
		cmd := []string{"postgres"}
		if m := regexp.MustCompile(`max_connections[= ](\d+)`).FindStringSubmatch(ci.text.String()); m != nil {
			cmd = append(cmd, "-c", "max_connections="+m[1])
		}
		cmd = append(cmd, "-c", "fsync=off")
		d.Services = append(d.Services, Service{Name: "postgres", Image: "postgres:" + ci.postgres, Port: 5432,
			Env: []KV{{Key: "POSTGRES_PASSWORD", Value: "postgres"}}, Command: cmd,
			Why: "CI installs postgresql-" + ci.postgres + "; a throwaway server next to every machine (127.0.0.1:5432)"})
	}
	for _, t := range sortedKeys(ci.tools) {
		if st, ok := knownTools[t]; ok {
			d.Setup = append(d.Setup, st)
			continue
		}
		d.Notes = append(d.Notes, fmt.Sprintf("tool %s (installed in %s) not added: add a setup step if remote runs need it", t, ci.tools[t]))
	}
	d.Notes = append(d.Notes, ci.dropped...)
	for _, u := range ci.ignored {
		d.Notes = append(d.Notes, "action "+u+" not translated")
	}
	if prefixes := envPatterns(ci.stepEnv); len(prefixes) > 0 && d.Rust {
		d.CargoEnv = appendUnique(d.CargoEnv, prefixes...)
		d.EnvWhy = "test variables set by CI steps: " + strings.Join(prefixes, ", ")
	}
}

// baseImage turns a pinned CI image (rust:1.98.1-trixie) into its slim,
// unpinned variant when a toolchain file pins the version anyway.
func baseImage(img string, toolchain bool) string {
	rest, ok := strings.CutPrefix(img, "rust:")
	if !ok || !toolchain {
		return img
	}
	ver, distro, _ := strings.Cut(rest, "-")
	if distro == "" && !regexp.MustCompile(`^\d`).MatchString(ver) {
		return img
	}
	distro = strings.TrimPrefix(distro, "slim-")
	if distro == "" || distro == "slim" {
		return "rust:1-slim"
	}
	return "rust:1-slim-" + distro
}

// envPatterns condenses variable names: names sharing a two-part prefix
// (APP_TEST_PROFILE, APP_TEST_PG_BIN) become APP_TEST_*.
func envPatterns(names []string) []string {
	byPrefix := map[string][]string{}
	var order []string
	for _, n := range names {
		parts := strings.SplitN(n, "_", 3)
		key := n
		if len(parts) == 3 {
			key = parts[0] + "_" + parts[1] + "_*"
		}
		if _, ok := byPrefix[key]; !ok {
			order = append(order, key)
		}
		byPrefix[key] = appendUnique(byPrefix[key], n)
	}
	var out []string
	for _, k := range order {
		if strings.HasSuffix(k, "*") && len(byPrefix[k]) == 1 {
			out = appendUnique(out, byPrefix[k][0])
		} else {
			out = appendUnique(out, k)
		}
	}
	return out
}

var gitDepRe = regexp.MustCompile(`git\s*=\s*"((?:https://|ssh://git@)github\.com[/:][^"]+)"`)

// detectGitDeps finds git dependencies on github.com, which may be private:
// cargo fetches them with git and the GH_TOKEN secret.
func (d *Detection) detectGitDeps() {
	wsDir := filepath.Join(d.Root, d.Workspace)
	filepath.WalkDir(wsDir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() {
			if p != wsDir && skipDir(de.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if de.Name() != "Cargo.toml" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, m := range gitDepRe.FindAllStringSubmatch(string(data), -1) {
			d.GitDeps = appendUnique(d.GitDeps, strings.TrimSuffix(m[1], ".git"))
		}
		return nil
	})
	sort.Strings(d.GitDeps)
	if len(d.GitDeps) == 0 {
		return
	}
	d.Secrets = append(d.Secrets, KV{Key: "GH_TOKEN", Value: "cmd:gh auth token",
		Why: "git dependencies on github.com (possibly private), resolved locally per run"})
	d.Setup = append(d.Setup, Step{Comment: "git dependencies on github.com: GH_TOKEN (see [secrets]) at fetch time", Run: credentialHelperStep})
	if !slices.ContainsFunc(d.Env, func(e KV) bool { return e.Key == "CARGO_NET_GIT_FETCH_WITH_CLI" }) {
		d.Env = append(d.Env, KV{Key: "CARGO_NET_GIT_FETCH_WITH_CLI", Value: "true", Why: "cargo fetches git dependencies with git and its credential helper"})
	}
	d.Apt = appendUnique(d.Apt, "git")
}

// detectTestTools adds environment allowlists for test tools in use.
func (d *Detection) detectTestTools(ci *ciFacts) {
	ws := filepath.Join(d.Root, d.Workspace)
	_, nextestCfg := os.Stat(filepath.Join(ws, ".config", "nextest.toml"))
	if _, ok := ci.tools["cargo-nextest"]; ok || nextestCfg == nil {
		d.CargoEnv = appendUnique(d.CargoEnv, "NEXTEST_*")
		if !slices.ContainsFunc(d.Setup, func(s Step) bool { return strings.Contains(s.Run, "nexte.st") }) {
			d.Setup = append(d.Setup, knownTools["cargo-nextest"])
		}
	}
	if lock, err := os.ReadFile(filepath.Join(ws, "Cargo.lock")); err == nil && strings.Contains(string(lock), "name = \"insta\"") {
		d.CargoEnv = appendUnique(d.CargoEnv, "INSTA_*")
	}
}

// finishPackages adds what the setup steps need themselves.
func (d *Detection) finishPackages() {
	all := ""
	for _, s := range d.Setup {
		all += s.Run + "\n"
	}
	if strings.Contains(all, "curl ") {
		d.Apt = appendUnique(d.Apt, "curl", "ca-certificates")
	}
	if regexp.MustCompile(`tar -[a-zA-Z]*J`).MatchString(all) || strings.Contains(all, ".tar.xz") {
		d.Apt = appendUnique(d.Apt, "xz-utils")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
