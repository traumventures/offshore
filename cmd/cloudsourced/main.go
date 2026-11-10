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

	"github.com/traumventures/cloudsourced/internal/client"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/envbuild"
	"github.com/traumventures/cloudsourced/internal/hostagent"
	"github.com/traumventures/cloudsourced/internal/logs"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/proxy"
	"github.com/traumventures/cloudsourced/internal/sandbox"
	"github.com/traumventures/cloudsourced/internal/server"
	"github.com/traumventures/cloudsourced/internal/transport"
)

var version = "dev"

const usage = `cloudsourced - run builds on cloud machines as if they ran locally

Usage:
  cloudsourced run [--] <tool> [args...]   run a command through the proxy
  cloudsourced shim install [tools...]     install proxies into ~/.cloudsourced/bin
  cloudsourced shim uninstall              remove proxies
  cloudsourced status [--json]             show machines, sessions and cost today
  cloudsourced why [N|last|list] [--json]  explain a recent run (placement, timings)
  cloudsourced metrics                     daemon, fleet and machine metrics (Prometheus text)
  cloudsourced fetch <glob>...             copy outputs from the latest remote build
  cloudsourced outputs mount|unmount|status
                                           mount the remote build directories (target/)
                                           read-only at their local path (output FUSE)
  cloudsourced down                        destroy all machines and caches
  cloudsourced reconcile                   reload the fleet configuration, apply the policy now
  cloudsourced daemon run|stop|status      manage the local daemon
  cloudsourced env [hash|dockerfile]       show the environment image of this repo
  cloudsourced editor [--mode local|lsp-remote] [vscode|zed|neovim]
                                           rust-analyzer settings for remote checks
  cloudsourced config check [--fleet FILE] validate the configuration files of this mode
  cloudsourced config show [--fleet FILE]  print the effective configuration (secrets redacted)
  cloudsourced controlplane [flags]        serve a shared control plane (see --help)
  cloudsourced sim [flags] <trace>...      simulate the fleet policy on a build trace
  cloudsourced controlplane info|client-config|health
  cloudsourced version

Internal:
  cloudsourced server                      machine server (started by providers)
  cloudsourced host [--spec F] [--state D] host agent on VM machines (systemd, root)
  cloudsourced sandbox-init                session namespace init

Environment:
  CLOUDSOURCED_DISABLE=1          always run locally
  CLOUDSOURCED_REQUIRE_REMOTE=1   fail instead of falling back to local execution
  CLOUDSOURCED_STDIN=always|never stdin forwarding policy
  CLOUDSOURCED_VERBOSE=1          print the machine used for each run
  CLOUDSOURCED_PORTS=8080,3000:3001  forward local ports into the session
  CLOUDSOURCED_CONFIG=FILE        client configuration (default ~/.config/cloudsourced/config.toml)
  CLOUDSOURCED_FLEET_CONFIG=FILE  fleet configuration (default: the client configuration file)
  CLOUDSOURCED_LOG_FORMAT=text|json  log format of daemon, control plane and machines
  CLOUDSOURCED_LOG_LEVEL=debug|info|warn|error  log level (CLOUDSOURCED_DEBUG=1: debug)
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
	case "server":
		log := logger() // before the CLOUDSOURCED_* variables are dropped
		cfg, err := server.ConfigFromEnv()
		if err != nil {
			return err
		}
		// The provider-supplied secrets must not leak into sessions.
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "CLOUDSOURCED_") {
				os.Unsetenv(k)
			}
		}
		s, err := server.New(cfg, log)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return s.Run(ctx)
	case "host":
		return hostCmd(args)
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
	case "daemon":
		return daemonCmd(args)
	case "status":
		return status(args)
	case "why":
		return why(args)
	case "metrics":
		return metricsCmd(args)
	case "down":
		if err := client.EnsureDaemon(context.Background()); err != nil {
			return err
		}
		_, err := client.Call(context.Background(), client.Request{Op: "down"}, os.Stderr)
		if err == nil {
			fmt.Println("all machines and caches destroyed")
		}
		return err
	case "reconcile":
		if err := client.EnsureDaemon(context.Background()); err != nil {
			return err
		}
		_, err := client.Call(context.Background(), client.Request{Op: "reconcile"}, os.Stderr)
		return err
	case "shim":
		return shimCmd(args)
	case "fetch":
		return fetch(args)
	case "debug":
		return debugCmd(args)
	case "env":
		return envCmd(args)
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
		d, err := client.NewDaemon(logger())
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return d.Serve(ctx)
	case "start":
		return client.EnsureDaemon(context.Background())
	case "stop":
		raw, err := client.Call(context.Background(), client.Request{Op: "ping"}, nil)
		if err != nil {
			fmt.Println("daemon not running")
			return nil
		}
		var info struct{ PID int }
		json.Unmarshal(raw, &info)
		client.Call(context.Background(), client.Request{Op: "shutdown"}, nil)
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
		raw, err := client.Call(context.Background(), client.Request{Op: "ping"}, nil)
		if err != nil {
			fmt.Println("daemon not running")
			return exitCode(3)
		}
		fmt.Printf("daemon running %s (socket %s, log %s)\n", raw, client.SocketPath(), client.LogPath())
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
	if err := client.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := client.Call(ctx, client.Request{Op: "status"}, os.Stderr)
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
			fmt.Printf("  %s session %s: %s (%s, running %s)\n", m.ID, s.ID, strings.Join(s.Argv, " "), s.Repo,
				time.Since(time.Unix(s.Started, 0)).Round(time.Second))
		}
	}
	return nil
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
		return errors.New("usage: cloudsourced shim install [tools...] | uninstall | path")
	}
	dir := ShimDir()
	switch args[0] {
	case "path":
		fmt.Println(dir)
		return nil
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
		if !strings.Contains(":"+os.Getenv("PATH")+":", ":"+dir+":") {
			fmt.Printf("\nAdd the shims to the front of your PATH:\n  export PATH=%q:$PATH\n", dir)
		}
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
	return errors.New("usage: cloudsourced shim install [tools...] | uninstall | path")
}

func envCmd(args []string) error {
	root, err := config.FindRepo(".")
	if err != nil {
		return fmt.Errorf("no %s found", config.RepoFileName)
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
	if err := client.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := client.Call(ctx, client.Request{Op: "prepare", Root: root}, os.Stderr)
	if err != nil {
		return err
	}
	var prep client.Prepared
	if err := json.Unmarshal(raw, &prep); err != nil {
		return err
	}
	defer client.Call(ctx, client.Request{Op: "release", Lease: prep.Lease}, nil)
	conn, err := transport.Dial(ctx, prep.Addr, prep.Fingerprint)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")
	_, pc, err := transport.OpenStream(ctx, conn, prep.Token, proto.KindFetch)
	if err != nil {
		return err
	}
	if err := pc.Send(proto.TFetch, proto.Fetch{RepoID: prep.RepoID, Repo: root, Paths: globs}); err != nil {
		return err
	}
	var outs proto.Outputs
	if err := pc.Expect(proto.TOutputs, &outs); err != nil {
		return err
	}
	if err := proxy.ReceiveFiles(pc, root, outs.Files); err != nil {
		return err
	}
	if err := pc.Write(proto.TDone, nil); err != nil {
		return err
	}
	for _, f := range outs.Files {
		fmt.Println(filepath.Join(root, f.Path))
	}
	if len(outs.Files) == 0 {
		return errors.New("no matching outputs on the machine")
	}
	return nil
}

// hostCmd runs the host agent of a VM machine.
func hostCmd(args []string) error {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	specPath := fs.String("spec", hostagent.SpecPath, "public machine spec")
	stateDir := fs.String("state", hostagent.StateDir, "root-only agent state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := hostagent.LoadSpec(*specPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	plat, err := hostagent.OpenPlatform(ctx, spec)
	if err != nil {
		return err
	}
	clk, err := clock.FromEnv()
	if err != nil {
		return err
	}
	a := &hostagent.Agent{
		Spec: spec, Platform: plat, StateDir: *stateDir, Docker: hostagent.DockerCLI("docker"), Log: logger(), Clock: clk,
		Shutdown: func() error { return exec.Command("shutdown", "-h", "now").Run() },
	}
	return a.Run(ctx)
}
