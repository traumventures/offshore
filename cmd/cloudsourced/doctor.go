package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/daemon"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/pool"
)

// Check states of `cloudsourced doctor`.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

// check is one result of `cloudsourced doctor`.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// doctorReport is the --json output.
type doctorReport struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

func doctorCmd(args []string, stdout io.Writer) error {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return errors.New("usage: cloudsourced doctor [--json]")
		}
		asJSON = true
	}
	checks := runDoctor()
	return printDoctor(stdout, checks, asJSON)
}

// printDoctor writes the report; it returns exit code 1 when a check failed.
func printDoctor(w io.Writer, checks []check, asJSON bool) error {
	rep := doctorReport{OK: true, Checks: checks}
	for _, c := range checks {
		if c.Status == checkFail {
			rep.OK = false
		}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		marks := map[string]string{checkOK: "ok  ", checkWarn: "WARN", checkFail: "FAIL", checkSkip: "--  "}
		for _, c := range checks {
			fmt.Fprintf(w, "[%s] %-16s %s\n", marks[c.Status], c.Name, c.Detail)
			if c.Hint != "" {
				fmt.Fprintf(w, "       %-16s %s\n", "", c.Hint)
			}
		}
	}
	if !rep.OK {
		return exitCode(1)
	}
	return nil
}

func runDoctor() []check {
	ctx := context.Background()
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	path := os.Getenv("PATH")
	checks := []check{checkBinary(self, path), checkShims(ShimDir(), path, self)}

	client, err := config.LoadClient()
	checks = append(checks, checkClient(client, err))
	var remote *pool.Remote
	var machines []orchestrator.Machine
	machinesKnown := false
	if err == nil {
		if client.Embedded() {
			checks = append(checks, checkEmbedded())
			checks = append(checks, check{Name: "clock", Status: checkSkip, Detail: "embedded mode: no control plane to compare with"})
		} else {
			var cs []check
			cs, remote = checkControlPlane(ctx, client, time.Now)
			checks = append(checks, cs...)
			if remote != nil {
				sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				machines, err = remote.Status(sctx)
				cancel()
				machinesKnown = err == nil
			}
		}
	}
	_, perr := daemon.Call(ctx, daemon.Request{Op: "ping"}, nil)
	checks = append(checks, checkDaemon(perr))
	if !machinesKnown && perr == nil && client != nil && client.Embedded() {
		if raw, err := daemon.Call(ctx, daemon.Request{Op: "status"}, nil); err == nil && json.Unmarshal(raw, &machines) == nil {
			machinesKnown = true
		}
	}
	checks = append(checks, checkCapacity(client, machines, machinesKnown))

	root, ferr := config.FindRepo(".")
	var repo *config.Repo
	if ferr != nil {
		checks = append(checks, check{Name: "repo config", Status: checkSkip, Detail: "no " + config.RepoFileName + " here or above",
			Hint: "cloudsourced init prints one for this repository"})
	} else {
		repo, err = config.LoadRepo(root)
		if err != nil {
			checks = append(checks, check{Name: "repo config", Status: checkFail, Detail: err.Error(),
				Hint: "commands run locally until the configuration is valid"})
		} else {
			checks = append(checks, check{Name: "repo config", Status: checkOK, Detail: filepath.Join(root, config.RepoFileName)})
		}
	}
	if repo != nil {
		checks = append(checks, checkSecrets(repo)...)
		checks = append(checks, checkEnvImage(ctx, repo, remote, perr == nil))
	}
	needFuse := repo != nil && repo.Outputs.Mode == config.OutputsFuse
	checks = append(checks, checkFuse("/dev/fuse", exec.LookPath, needFuse))
	return checks
}

// lookPathIn finds the first executable named name in path, like
// exec.LookPath but for an explicit PATH value.
func lookPathIn(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func sameFile(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func checkBinary(self, path string) check {
	c := check{Name: "binary"}
	found := lookPathIn("cloudsourced", path)
	switch {
	case found == "":
		c.Status, c.Detail = checkWarn, "cloudsourced is not in PATH (running "+self+")"
		c.Hint = "put its directory into PATH so scripts and editors find it"
	case !sameFile(found, self):
		c.Status, c.Detail = checkWarn, "PATH resolves cloudsourced to "+found+", not to this binary "+self
		c.Hint = "remove the stale copy or reorder PATH"
	default:
		c.Status, c.Detail = checkOK, found+" ("+version+")"
	}
	return c
}

// checkShims verifies that every installed shim is what PATH finds first
// for its tool.
func checkShims(dir, path, self string) check {
	c := check{Name: "shims"}
	des, _ := os.ReadDir(dir)
	var tools []string
	for _, de := range des {
		if sameFile(filepath.Join(dir, de.Name()), self) {
			tools = append(tools, de.Name())
		}
	}
	sort.Strings(tools)
	if len(tools) == 0 {
		c.Status, c.Detail = checkWarn, "no shims in "+dir+": tools run locally unless called as `cloudsourced run`"
		c.Hint = "cloudsourced shim install"
		return c
	}
	var bad []string
	for _, t := range tools {
		first := lookPathIn(t, path)
		if first == "" || filepath.Dir(first) != filepath.Clean(dir) {
			if first == "" {
				first = "nothing"
			}
			bad = append(bad, fmt.Sprintf("%s resolves to %s", t, first))
		}
	}
	if len(bad) > 0 {
		c.Status, c.Detail = checkFail, strings.Join(bad, "; ")
		c.Hint = fmt.Sprintf("put the shims first in PATH: export PATH=%q:$PATH", dir)
		return c
	}
	c.Status, c.Detail = checkOK, fmt.Sprintf("%s first in PATH (%s)", dir, strings.Join(tools, ", "))
	return c
}

func checkClient(c *config.Client, err error) check {
	if err != nil {
		return check{Name: "client config", Status: checkFail, Detail: err.Error()}
	}
	r := check{Name: "client config", Status: checkOK}
	src := c.Path
	if _, err := os.Stat(c.Path); err != nil {
		src = "defaults (" + c.Path + " absent)"
	}
	r.Detail = fmt.Sprintf("%s, %s mode, user %s", src, c.ControlPlane.Mode, c.User)
	if len(c.Warnings) > 0 {
		r.Status, r.Hint = checkWarn, strings.Join(c.Warnings, "; ")
	}
	return r
}

func checkEmbedded() check {
	path := config.PoolConfigPath()
	fc, err := config.LoadPool(path)
	if err != nil {
		return check{Name: "pool config", Status: checkFail, Detail: err.Error()}
	}
	c := check{Name: "pool config", Status: checkOK,
		Detail: fmt.Sprintf("embedded orchestrator, %s, providers %s", path, strings.Join(fc.ProviderNames(), ", "))}
	if len(fc.Warnings) > 0 {
		c.Status, c.Hint = checkWarn, strings.Join(fc.Warnings, "; ")
	}
	return c
}

// checkControlPlane checks reachability, the pinned certificate, the API
// key and the clock of a remote control plane. It returns the connected
// pool when the API key was accepted.
func checkControlPlane(ctx context.Context, c *config.Client, now func() time.Time) ([]check, *pool.Remote) {
	o := c.ControlPlane
	key, err := config.ResolveRef(o.APIKey, c.Dir())
	if err != nil {
		return []check{{Name: "control plane", Status: checkFail, Detail: "API key " + o.APIKey + ": " + err.Error(),
			Hint: "control_plane.api_key in " + c.Path}}, nil
	}
	var certs []tls.Certificate
	if o.ClientCert != "" {
		abs := func(p string) string {
			if filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(c.Dir(), p)
		}
		cert, err := tls.LoadX509KeyPair(abs(o.ClientCert), abs(o.ClientKey))
		if err != nil {
			return []check{{Name: "control plane", Status: checkFail, Detail: "client certificate: " + err.Error()}}, nil
		}
		certs = append(certs, cert)
	}
	r, err := pool.NewRemote(o.URL, o.Fingerprint, key, certs...)
	if err != nil {
		return []check{{Name: "control plane", Status: checkFail, Detail: err.Error()}}, nil
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	before := now()
	serverTime, err := r.Health(hctx)
	after := now()
	cancel()
	if err != nil {
		c := check{Name: "control plane", Status: checkFail, Detail: o.URL + " unreachable: " + err.Error()}
		if strings.Contains(err.Error(), "fingerprint mismatch") {
			c.Detail = o.URL + ": the certificate does not match control_plane.fingerprint"
			c.Hint = "the control plane was recreated or this is not it: compare with `cloudsourced controlplane info` on the server"
		} else {
			c.Hint = "is the control plane running and the URL reachable from here (VPN, firewall)?"
		}
		return []check{c, {Name: "clock", Status: checkSkip, Detail: "control plane unreachable"}}, nil
	}
	pin := "verified against system CAs"
	if o.Fingerprint != "" {
		pin = "certificate fingerprint matches"
	}
	out := []check{{Name: "control plane", Status: checkOK, Detail: o.URL + " reachable, " + pin}}
	out = append(out, checkClock(serverTime, before.Add(after.Sub(before)/2)))
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, err = r.Status(sctx)
	cancel()
	if err != nil {
		k := check{Name: "api key", Status: checkFail, Detail: err.Error()}
		if strings.Contains(err.Error(), "401") {
			k.Detail = "the control plane rejects the API key (" + o.APIKey + ")"
			k.Hint = "get the current key with `cloudsourced controlplane info --show-key` on the server"
		}
		return append(out, k), nil
	}
	return append(out, check{Name: "api key", Status: checkOK, Detail: "accepted (" + o.APIKey + ")"}), r
}

// checkClock compares the control plane's clock (HTTP Date, 1 s
// resolution) with ours. Leases, build slot expiry and TLS depend on it.
func checkClock(server, local time.Time) check {
	c := check{Name: "clock"}
	if server.IsZero() {
		c.Status, c.Detail = checkSkip, "the control plane sent no Date header"
		return c
	}
	skew := local.Sub(server)
	abs := skew
	if abs < 0 {
		abs = -abs
	}
	c.Detail = fmt.Sprintf("skew %s against the control plane", skew.Round(time.Second))
	switch {
	case abs <= 5*time.Second:
		c.Status = checkOK
	case abs <= time.Minute:
		c.Status, c.Hint = checkWarn, "enable time synchronisation (timedatectl set-ntp true)"
	default:
		c.Status, c.Hint = checkFail, "the clock is off by more than a minute: enable time synchronisation (timedatectl set-ntp true)"
	}
	return c
}

func checkDaemon(pingErr error) check {
	if pingErr != nil {
		return check{Name: "daemon", Status: checkWarn, Detail: "not running (socket " + daemon.SocketPath() + ")",
			Hint: "it starts with the first remote run; `cloudsourced daemon start` starts it now, log: " + daemon.LogPath()}
	}
	return check{Name: "daemon", Status: checkOK, Detail: "running (log " + daemon.LogPath() + ")"}
}

// checkCapacity reports the machines of the pool and, in embedded mode,
// its limits. Provider quotas are not visible to the client.
func checkCapacity(c *config.Client, ms []orchestrator.Machine, known bool) check {
	r := check{Name: "capacity", Status: checkOK}
	if !known {
		r.Status, r.Detail = checkSkip, "machine list unavailable"
		return r
	}
	up, ready := 0, 0
	byProv := map[string]int{}
	for _, m := range ms {
		if !m.Removed.IsZero() {
			continue
		}
		up++
		byProv[m.Provider]++
		if m.Status != nil && m.Status.Ready && !m.Status.Draining {
			ready++
		}
	}
	var parts []string
	for _, p := range sortedKeys(byProv) {
		parts = append(parts, fmt.Sprintf("%s %d", p, byProv[p]))
	}
	r.Detail = fmt.Sprintf("%d machines (%d ready)", up, ready)
	if len(parts) > 0 {
		r.Detail += ": " + strings.Join(parts, ", ")
	}
	if c != nil && c.Embedded() {
		if fc, err := config.LoadPool(config.PoolConfigPath()); err == nil && fc.Pool.MaxMachines > 0 {
			r.Detail += fmt.Sprintf("; max_machines %d", fc.Pool.MaxMachines)
			if up >= fc.Pool.MaxMachines {
				r.Status, r.Hint = checkWarn, "the pool is at max_machines: new work queues until a machine frees up"
			}
		}
	} else {
		r.Hint = "provider quotas are enforced by the control plane; see its log when acquires queue"
	}
	return r
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkSecrets resolves every secret without printing it.
func checkSecrets(repo *config.Repo) []check {
	if len(repo.Secrets) == 0 {
		return []check{{Name: "secrets", Status: checkSkip, Detail: "none configured"}}
	}
	names := make([]string, 0, len(repo.Secrets))
	for n := range repo.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []check
	for _, n := range names {
		ref := repo.Secrets[n]
		c := check{Name: "secret " + n}
		v, err := resolveQuiet(ref, repo.Root)
		switch {
		case err != nil:
			c.Status, c.Detail = checkFail, ref+": "+err.Error()
			c.Hint = "remote runs fall back to local execution while a secret does not resolve"
			if strings.HasPrefix(ref, "cmd:gh ") {
				c.Hint = "log in with `gh auth login`"
			}
		case v == "":
			c.Status, c.Detail = checkWarn, ref+" resolves to an empty value"
		default:
			c.Status, c.Detail = checkOK, fmt.Sprintf("%s resolves (%d characters)", ref, len(v))
		}
		out = append(out, c)
	}
	return out
}

// resolveQuiet resolves a secret reference without passing a command's
// stderr through.
func resolveQuiet(ref, dir string) (string, error) {
	kind, arg, _ := strings.Cut(ref, ":")
	if kind != "cmd" {
		return config.ResolveRef(ref, dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", arg)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg := strings.TrimSpace(string(ee.Stderr))
			if i := strings.IndexByte(msg, '\n'); i > 0 {
				msg = msg[:i]
			}
			return "", fmt.Errorf("%q failed: %s", arg, msg)
		}
		return "", fmt.Errorf("%q failed: %w", arg, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// checkEnvImage reports the environment hash and whether its image exists.
func checkEnvImage(ctx context.Context, repo *config.Repo, remote *pool.Remote, daemonUp bool) check {
	c := check{Name: "environment"}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		c.Status, c.Detail = checkFail, err.Error()
		return c
	}
	st := &daemon.EnvStatus{Hash: plan.Hash, Tag: plan.Tag}
	switch {
	case daemonUp:
		raw, err := daemon.Call(ctx, daemon.Request{Op: "env-status", Root: repo.Root}, nil)
		if err == nil {
			json.Unmarshal(raw, st)
		} else {
			st.PoolErr = err.Error()
		}
	case remote != nil:
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ps, err := remote.EnvState(sctx, plan.Hash, plan.Tag)
		cancel()
		if err != nil {
			st.PoolErr = err.Error()
		} else {
			st.Pool = &ps
		}
	default:
		st.PoolErr = "daemon not running"
	}
	c.Status, c.Detail = checkOK, plan.Tag+": "+describeEnvState(st)
	if b := st.Prebuild; b != nil && b.Error != "" && !b.Running {
		c.Status, c.Hint = checkWarn, "last background build failed: "+b.Error+" (cloudsourced env build --wait shows the output)"
	}
	return c
}

// checkFuse checks what the output FUSE (and the source projection's
// machines) need locally: /dev/fuse and fusermount3. A failure only
// matters when the repository uses [outputs] mode = "fuse".
func checkFuse(dev string, lookPath func(string) (string, error), needed bool) check {
	c := check{Name: "fuse"}
	var missing []string
	if f, err := os.OpenFile(dev, os.O_RDWR, 0); err != nil {
		missing = append(missing, err.Error())
	} else {
		f.Close()
	}
	fm, err := lookPath("fusermount3")
	if err != nil {
		missing = append(missing, "fusermount3 not in PATH")
	}
	if len(missing) == 0 {
		c.Status, c.Detail = checkOK, dev+" and "+fm+" available"
		return c
	}
	c.Detail = strings.Join(missing, "; ")
	c.Hint = "install fuse3 (apt install fuse3) for `cloudsourced outputs mount`"
	if needed {
		c.Status = checkFail
		c.Hint += `; this repository uses [outputs] mode = "fuse"`
	} else {
		c.Status = checkWarn
	}
	return c
}
