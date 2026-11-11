// Command cloudsourced runs build and test commands on cloud machines as if
// they ran locally. Invoked through a symlink (e.g. ~/.cloudsourced/bin/cargo)
// it acts as a transparent proxy for that tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/traumventures/cloudsourced/internal/clock"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/daemon"
	"github.com/traumventures/cloudsourced/internal/labels"
	"github.com/traumventures/cloudsourced/internal/logs"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/proxy"
	"github.com/traumventures/cloudsourced/internal/sandbox"
	"github.com/traumventures/cloudsourced/internal/supervisor"
	"github.com/traumventures/cloudsourced/internal/tracing"
	"github.com/traumventures/cloudsourced/internal/transport"
	"github.com/traumventures/cloudsourced/internal/worker"
)

var version = "dev"

const usage = `cloudsourced - run builds on cloud machines as if they ran locally

Usage:
  cloudsourced run [--] <tool> [args...]   run a command through the proxy
  cloudsourced shim install [tools...]     install proxies into ~/.cloudsourced/bin
  cloudsourced shim status                 are the proxies first in PATH (exit 1 if not)
  cloudsourced shim uninstall              remove proxies
  cloudsourced status [--json]             show machines, sessions and cost today
  cloudsourced why [N|last|list] [--json]  explain a recent run (placement, timings, labels)
  cloudsourced top [--once] [--window 10m] live view: machines, sessions, queue, cost
  cloudsourced prewarm [--wait] [--force]  start the remote environment of this repo in the
                                           background ([prewarm] run = [...] warms the build)
  cloudsourced hooks install|uninstall|status
                                           git post-checkout hook: prewarm on new worktrees
  cloudsourced metrics                     daemon, pool and machine metrics (Prometheus text)
  cloudsourced fetch <glob>...             copy outputs from the latest remote build
  cloudsourced outputs mount|unmount|status
                                           mount the remote build directories (target/)
                                           at their local path as a writable overlay:
                                           local cargo finds remote builds fresh
  cloudsourced down                        destroy all machines and caches
  cloudsourced reconcile                   reload the pool configuration, apply the policy now
  cloudsourced daemon run|stop|status      manage the local daemon
  cloudsourced env [hash|dockerfile]       show the environment image of this repo
  cloudsourced env build [--wait]          build the environment image now (in the background)
  cloudsourced env status [--json]         where the environment image exists, last prebuild
  cloudsourced init [--write [--force]]    generate .cloudsourced.toml from Cargo, CI and Dockerfiles
  cloudsourced doctor [--json]             check installation, configuration and connectivity
  cloudsourced editor [--mode local|lsp-remote] [vscode|zed|neovim]
                                           rust-analyzer settings for remote checks
  cloudsourced config check [--pool FILE] validate the configuration files of this mode
  cloudsourced config show [--pool FILE]  print the effective configuration (secrets redacted)
  cloudsourced controlplane [flags]        serve a shared control plane (see --help)
  cloudsourced sim [flags] <trace>...      simulate the pool policy on a build trace
  cloudsourced sim export [--since 7d] -o trace.json
                                           convert the event log into a simulator trace
  cloudsourced report [--since 7d] [--by agent|worktree|branch|repo|user] [--json]
                                           cost, utilisation, queue wait, cache, estimator
                                           accuracy and per-label groups from the event log
  cloudsourced controlplane info|client-config|health
  cloudsourced version

Internal:
  cloudsourced worker                      machine worker (started by providers)
  cloudsourced supervisor [--spec F] [--state D]
                                           supervisor on VM machines (systemd, root)
  cloudsourced sandbox-init                session namespace init

Environment:
  CLOUDSOURCED_DISABLE=1          always run locally
  CLOUDSOURCED_REQUIRE_REMOTE=1   fail instead of falling back to local execution
  CLOUDSOURCED_STDIN=always|never stdin forwarding policy
  CLOUDSOURCED_AGENT=NAME         agent label of sessions (default: detected, see README)
  CLOUDSOURCED_LABELS=k=v,k=v     extra session labels (status, top, why, report --by)
  CLOUDSOURCED_VERBOSE=1          print the machine used for each run
  CLOUDSOURCED_NO_RETRY=1         no second attempt when the machine is lost mid-run
  CLOUDSOURCED_SPLIT=0            run cargo test / nextest entirely remotely (no local test run)
  CLOUDSOURCED_PORTS=8080,3000:3001  forward local ports into the session
  CLOUDSOURCED_CONFIG=FILE        client configuration (default ~/.config/cloudsourced/config.toml)
  CLOUDSOURCED_POOL_CONFIG=FILE  pool configuration (default: the client configuration file)
  CLOUDSOURCED_LOG_FORMAT=text|json  log format of daemon, control plane and machines
  CLOUDSOURCED_LOG_LEVEL=debug|info|warn|error  log level (CLOUDSOURCED_DEBUG=1: debug)
  OTEL_TRACES_EXPORTER=otlp|console|none, OTEL_EXPORTER_OTLP_ENDPOINT=URL
                                  OpenTelemetry tracing (off by default; see [telemetry])
`

func init() {
	// quic-go warns when the kernel caps UDP buffers; that is not actionable
	// for users of a transparent proxy.
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
}

func main() {
	name := filepath.Base(os.Args[0])
	if name != "cloudsourced" && !strings.HasPrefix(name, "cloudsourced.") && !strings.HasSuffix(name, ".test") {
		os.Exit(proxy.Main(name, os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "cloudsourced:", err)
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit code %d", int(e)) }

// logger logs to stderr in the format of CLOUDSOURCED_LOG_FORMAT and
// CLOUDSOURCED_LOG_LEVEL (see internal/logs).
func logger() *slog.Logger { return logs.FromEnv() }

func run(cmd string, args []string) error {
	switch cmd {
	case "run":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			return errors.New("run: missing command")
		}
		os.Exit(proxy.Main(args[0], args[1:]))
	case "worker":
		log := logger() // before the CLOUDSOURCED_* variables are dropped
		defer flushTelemetry(tracing.SetupFromConfig(tracing.ServiceWorker, config.Telemetry{}, "", log))
		cfg, err := worker.ConfigFromEnv()
		if err != nil {
			return err
		}
		// The provider-supplied secrets must not leak into sessions.
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "CLOUDSOURCED_") {
				os.Unsetenv(k)
			}
		}
		s, err := worker.New(cfg, log)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return s.Run(ctx)
	case "supervisor":
		return supervisorCmd(args)
	case "sandbox-init":
		err := sandbox.Init()
		fmt.Fprintln(os.Stderr, err)
		if strings.HasSuffix(err.Error(), "command not found") {
			os.Exit(127)
		}
		os.Exit(126)
	case "controlplane":
		return controlplane(args)
	case "sim":
		return simCmd(args)
	case "report":
		return reportCmd(args)
	case "daemon":
		return daemonCmd(args)
	case "status":
		return status(args)
	case "top":
		return topCmd(args)
	case "prewarm":
		return prewarmCmd(args)
	case "hooks":
		return hooksCmd(args)
	case "why":
		return why(args)
	case "metrics":
		return metricsCmd(args)
	case "down":
		if err := daemon.EnsureDaemon(context.Background()); err != nil {
			return err
		}
		_, err := daemon.Call(context.Background(), daemon.Request{Op: "down"}, os.Stderr)
		if err == nil {
			fmt.Println("all machines and caches destroyed")
		}
		return err
	case "reconcile":
		if err := daemon.EnsureDaemon(context.Background()); err != nil {
			return err
		}
		_, err := daemon.Call(context.Background(), daemon.Request{Op: "reconcile"}, os.Stderr)
		return err
	case "shim":
		return shimCmd(args)
	case "fetch":
		return fetch(args)
	case "debug":
		return debugCmd(args)
	case "env":
		return envCmd(args)
	case "init":
		return initCmd(args, os.Stdout)
	case "doctor":
		return doctorCmd(args, os.Stdout)
	case "editor":
		return editorCmd(args)
	case "outputs":
		return outputsCmd(args)
	case "config":
		return configCmd(args, os.Stdout)
	case "version", "--version":
		fmt.Println("cloudsourced", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see cloudsourced help)", cmd)
}

func daemonCmd(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		log := logger()
		if c, err := config.LoadClient(); err == nil {
			defer flushTelemetry(tracing.SetupFromConfig(tracing.ServiceDaemon, c.Telemetry, c.Dir(), log))
		}
		d, err := daemon.NewDaemon(log)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return d.Serve(ctx)
	case "start":
		return daemon.EnsureDaemon(context.Background())
	case "stop":
		raw, err := daemon.Call(context.Background(), daemon.Request{Op: "ping"}, nil)
		if err != nil {
			fmt.Println("daemon not running")
			return nil
		}
		var info struct{ PID int }
		json.Unmarshal(raw, &info)
		daemon.Call(context.Background(), daemon.Request{Op: "shutdown"}, nil)
		// Wait for the process to exit, not just for the socket to close.
		for i := 0; i < 200 && info.PID > 0; i++ {
			if syscall.Kill(info.PID, 0) != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Println("daemon stopped")
		return nil
	case "status":
		raw, err := daemon.Call(context.Background(), daemon.Request{Op: "ping"}, nil)
		if err != nil {
			fmt.Println("daemon not running")
			return exitCode(3)
		}
		fmt.Printf("daemon running %s (socket %s, log %s)\n", raw, daemon.SocketPath(), daemon.LogPath())
		return nil
	}
	return errors.New("usage: cloudsourced daemon run|start|stop|status")
}

// statusJSON is the output of `cloudsourced status --json`.
type statusJSON struct {
	Machines []orchestrator.Machine  `json:"machines"` // includes machines removed today (Removed set)
	Cost     orchestrator.CostReport `json:"cost"`
}

func status(args []string) error {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			return errors.New("usage: cloudsourced status [--json]")
		}
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "status"}, os.Stderr)
	if err != nil {
		return err
	}
	var all []orchestrator.Machine
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	now := time.Now()
	cost := orchestrator.Costs(all, orchestrator.StartOfDay(now), now)
	if asJSON {
		if all == nil {
			all = []orchestrator.Machine{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(statusJSON{Machines: all, Cost: cost})
	}
	var ms []orchestrator.Machine
	for _, m := range all {
		if m.Removed.IsZero() {
			ms = append(ms, m)
		}
	}
	defer printCost(cost)
	if len(ms) == 0 {
		fmt.Println("no machines")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tPROVIDER\tSIZE\tSTATE\tENV\tADDR\tCPU\tMEMORY\tSESSIONS\tAGE\tEXPIRES IN")
	for _, m := range ms {
		state := string(m.State)
		sessions := "-"
		if m.Status != nil {
			if !m.Status.Ready {
				state += " (provisioning)"
			} else if m.Status.Draining {
				state += " (draining)"
			} else if len(m.Status.Sessions) == 0 && m.Status.IdleSince > 0 {
				state += fmt.Sprintf(" (idle %s)", time.Since(time.Unix(m.Status.IdleSince, 0)).Round(time.Second))
			}
			sessions = fmt.Sprint(len(m.Status.Sessions))
		}
		env := m.EnvHash
		if len(env) > 12 {
			env = env[:12]
		}
		if m.Draining && !strings.Contains(state, "draining") {
			state += " (draining)"
		}
		size := m.Labels[provider.LabelSize]
		if size == "" {
			size = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%g\t%s\t%s\t%s\t%s\n", m.ID, m.Provider, size, state, env, m.Addr, m.CPU, human(m.Memory), sessions,
			time.Since(m.CreatedAt).Round(time.Second), time.Until(m.ExpiresAt).Round(time.Minute))
	}
	tw.Flush()
	for _, m := range ms {
		if m.Status == nil {
			continue
		}
		sort.Slice(m.Status.Sessions, func(i, j int) bool { return m.Status.Sessions[i].Started < m.Status.Sessions[j].Started })
		for _, s := range m.Status.Sessions {
			where := s.Repo
			if l := sessionLabels(s.Labels); l != "" {
				where = l
			}
			fmt.Printf("  %s session %s: %s (%s, running %s)\n", m.ID, s.ID, strings.Join(s.Argv, " "), where,
				time.Since(time.Unix(s.Started, 0)).Round(time.Second))
		}
	}
	return nil
}

// sessionLabels renders the labels of a running session for `status`
// (without the user, which is the same for all of one's sessions).
func sessionLabels(l map[string]string) string {
	c := labels.Clean(l)
	delete(c, labels.User)
	return labels.Format(c)
}

// printCost prints the estimated cost per machine size since the start of
// the day and a total line.
func printCost(r orchestrator.CostReport) {
	if len(r.Sizes) == 0 {
		return
	}
	fmt.Printf("\ncost since %s (estimated from observed uptime):\n", r.Since.Format("15:04 MST"))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SIZE\tPRICE/H\tMACHINES\tUP NOW\tUPTIME\tCOST")
	unpriced := false
	for _, sc := range r.Sizes {
		price := fmt.Sprintf("%.3f", sc.PricePerHour)
		if sc.Unpriced {
			price, unpriced = "-", true
		}
		fmt.Fprintf(tw, "  %s/%s\t%s\t%d\t%d\t%s\t%.2f\n", sc.Provider, sc.Size, price, sc.Machines, sc.Running,
			(time.Duration(sc.UptimeSec) * time.Second).Round(time.Second), sc.Cost)
	}
	fmt.Fprintf(tw, "  total\t%.3f\t\t\t\t%.2f\n", r.PerHour, r.Total)
	tw.Flush()
	if unpriced {
		fmt.Println("  (\"-\": no price_per_hour configured for the size)")
	}
}

func human(b int64) string {
	const g = 1 << 30
	if b >= g {
		return fmt.Sprintf("%.1fG", float64(b)/g)
	}
	return fmt.Sprintf("%dM", b>>20)
}

// ShimDir is where proxy symlinks are installed.
func ShimDir() string {
	if d := os.Getenv("CLOUDSOURCED_SHIM_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.Getenv("HOME"), ".cloudsourced", "bin")
}

func shimCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cloudsourced shim install [tools...] | status | uninstall | path")
	}
	dir := ShimDir()
	switch args[0] {
	case "path":
		fmt.Println(dir)
		return nil
	case "status":
		return shimStatus()
	case "install":
		tools := args[1:]
		if len(tools) == 0 {
			tools = []string{"cargo"}
			if root, err := config.FindRepo("."); err == nil {
				if repo, err := config.LoadRepo(root); err == nil && len(repo.Tools) > 0 {
					tools = tools[:0]
					for t := range repo.Tools {
						tools = append(tools, t)
					}
					sort.Strings(tools)
				}
			}
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		self, _ = filepath.EvalSymlinks(self)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, t := range tools {
			link := filepath.Join(dir, t)
			os.Remove(link)
			if err := os.Symlink(self, link); err != nil {
				return err
			}
			fmt.Println("installed", link)
		}
		fmt.Println()
		r := inspectShims(dir, os.Getenv("PATH"), self)
		r.print(os.Stdout)
		return nil
	case "uninstall":
		des, _ := os.ReadDir(dir)
		self, _ := os.Executable()
		self, _ = filepath.EvalSymlinks(self)
		for _, de := range des {
			p := filepath.Join(dir, de.Name())
			if t, err := filepath.EvalSymlinks(p); err == nil && t == self {
				os.Remove(p)
				fmt.Println("removed", p)
			}
		}
		return nil
	}
	return errors.New("usage: cloudsourced shim install [tools...] | status | uninstall | path")
}

func envCmd(args []string) error {
	root, err := config.FindRepo(".")
	if err != nil {
		return fmt.Errorf("no %s found", config.RepoFileName)
	}
	if len(args) > 0 && (args[0] == "build" || args[0] == "status") {
		return envDaemonCmd(root, args[0], args[1:])
	}
	repo, err := config.LoadRepo(root)
	if err != nil {
		return err
	}
	plan, err := envbuild.Resolve(repo)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "dockerfile" {
		fmt.Print(plan.Dockerfile)
		return nil
	}
	if len(args) > 0 && args[0] == "hash" {
		fmt.Println(plan.Hash)
		return nil
	}
	fmt.Printf("hash:  %s\nimage: %s\n", plan.Hash, plan.Tag)
	return nil
}

// envDaemonCmd serves `env build` (prebuild through the daemon) and
// `env status`.
func envDaemonCmd(root, sub string, args []string) error {
	wait, asJSON := false, false
	for _, a := range args {
		switch {
		case a == "--wait" && sub == "build":
			wait = true
		case a == "--json" && sub == "status":
			asJSON = true
		default:
			return errors.New("usage: cloudsourced env build [--wait] | env status [--json]")
		}
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	if sub == "build" {
		raw, err := daemon.Call(ctx, daemon.Request{Op: "env-build", Root: root, Wait: wait}, os.Stderr)
		if err != nil {
			return err
		}
		var b daemon.Prebuild
		json.Unmarshal(raw, &b)
		switch {
		case wait:
			fmt.Printf("environment image %s ready\n", b.Tag)
		case b.Running:
			fmt.Printf("building environment image %s in the background (cloudsourced env status)\n", b.Tag)
		default:
			fmt.Printf("environment image %s already built\n", b.Tag)
		}
		return nil
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "env-status", Root: root}, os.Stderr)
	if err != nil {
		return err
	}
	var st daemon.EnvStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	fmt.Printf("hash:  %s\nimage: %s\n", st.Hash, st.Tag)
	fmt.Println("pool: ", describeEnvState(&st))
	if b := st.Prebuild; b != nil {
		state := "done"
		switch {
		case b.Running:
			state = "running for " + time.Since(b.Started).Round(time.Second).String()
		case b.Error != "":
			state = "failed: " + b.Error
		}
		fmt.Printf("prebuild: %s (%s, started %s)\n", state, b.Trigger, b.Started.Format(time.TimeOnly))
	}
	return nil
}

// describeEnvState explains where an environment image exists.
func describeEnvState(st *daemon.EnvStatus) string {
	switch p := st.Pool; {
	case st.PoolErr != "":
		return "unknown (" + st.PoolErr + ")"
	case p == nil:
		return "unknown (the pool does not report image state)"
	case p.Building:
		return "building now"
	case len(p.Missing) == 0 && len(p.Ready) > 0:
		return "image exists on " + strings.Join(p.Ready, ", ")
	case !p.Known:
		return "not known yet: the next run uploads the build context and builds the image (or run `cloudsourced env build`)"
	default:
		s := "will be built for " + strings.Join(p.Missing, ", ") + " when a machine needs it (or run `cloudsourced env build`)"
		if len(p.Ready) > 0 {
			s += "; exists on " + strings.Join(p.Ready, ", ")
		}
		if p.Error != "" {
			s += "; last prebuild failed: " + p.Error
		}
		return s
	}
}

// fetch copies output files of the most recent remote build into the
// local repository (lazy outputs).
func fetch(globs []string) error {
	if len(globs) == 0 {
		return errors.New("usage: cloudsourced fetch <glob>...")
	}
	ctx := context.Background()
	root, err := config.FindRepo(".")
	if err != nil {
		return fmt.Errorf("no %s found", config.RepoFileName)
	}
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "prepare", Root: root}, os.Stderr)
	if err != nil {
		return err
	}
	var prep daemon.Prepared
	if err := json.Unmarshal(raw, &prep); err != nil {
		return err
	}
	defer daemon.Call(ctx, daemon.Request{Op: "release", Lease: prep.Lease}, nil)
	conn, err := transport.Dial(ctx, prep.Addr, prep.Fingerprint)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")
	files, err := proxy.FetchOutputs(ctx, conn, prep.Token, prep.RepoID, root, globs, proto.CompressZstd)
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Println(filepath.Join(root, f.Path))
	}
	if len(files) == 0 {
		return errors.New("no matching outputs on the machine")
	}
	return nil
}

// supervisorCmd runs the supervisor of a VM machine.
func supervisorCmd(args []string) error {
	fs := flag.NewFlagSet("supervisor", flag.ContinueOnError)
	specPath := fs.String("spec", supervisor.SpecPath, "public machine spec")
	stateDir := fs.String("state", supervisor.StateDir, "root-only agent state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := supervisor.LoadSpec(*specPath)
	if err != nil {
		return err
	}
	log := logger()
	// Machines are told how to trace through their environment.
	defer flushTelemetry(tracing.SetupFromConfig(tracing.ServiceSupervisor, config.Telemetry{
		Exporter: spec.Layout.Env["OTEL_TRACES_EXPORTER"], Endpoint: spec.Layout.Env["OTEL_EXPORTER_OTLP_ENDPOINT"],
	}, "", log))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	plat, err := supervisor.OpenPlatform(ctx, spec)
	if err != nil {
		return err
	}
	clk, err := clock.FromEnv()
	if err != nil {
		return err
	}
	a := &supervisor.Supervisor{
		Spec: spec, Platform: plat, StateDir: *stateDir, Docker: supervisor.DockerCLI("docker"), Log: log, Clock: clk,
		Shutdown: func() error { return exec.Command("shutdown", "-h", "now").Run() },
	}
	return a.Run(ctx)
}
