# CLI

`offshore` dispatches on its name: invoked as anything other than
`offshore` (a shim such as `~/.offshore/bin/cargo`) it is the proxy
for that tool. `offshore help` prints the summary below; `offshore help
<command>` and `offshore <command> --help` (or `-h`) print the usage of one
command (flags included where it has any). `offshore run --help` is offshore's
help; `offshore run cargo --help` is cargo's.

## Running tools

| command | |
|---|---|
| `offshore run [--] <tool> [args...]` | run a command through the proxy (the explicit form of a shim) |
| `offshore shim install [tools...]` | install proxies into `~/.offshore/bin` (`$OFFSHORE_SHIM_DIR`); without tools: the `[tool.*]` sections of the current repository |
| `offshore shim status` | are the proxies first in `PATH` (exit 1 if not) |
| `offshore shim uninstall` | remove the proxies |
| `offshore shim path` | print the shim directory |
| `offshore fetch <glob>...` | copy outputs from the latest remote build of this repository (zstd) |
| `offshore outputs mount\|unmount\|status` | the output overlay of the slot cache directories ([tools.md](tools.md#output-overlay)) |
| `offshore editor [--mode local\|lsp-remote] vscode\|zed\|neovim` | rust-analyzer settings for remote checks ([tools.md](tools.md#rust-analyzer)) |

## Repository setup

### init

`offshore init [--write [--force]]` prints a `.offshore.toml` for the
current repository (git top level); `--write` writes it, `--force` replaces
an existing one. It is derived from what the repository already declares,
with a comment explaining every detected item:

* the ecosystems (tool adapters: cargo, go via `go.mod`/`go.work`, node via
  `package.json`) and their default caches as `[cache.<name>]`, plus
  `[tool.go]`/`[tool.<package manager>]` sections and a `golang:<go
  directive>-trixie` image, `oven/bun:1-debian` (bun) or
  `node:lts-trixie-slim` when there is no Rust;
* the Cargo workspace (repository root or a subdirectory), its
  `rust-toolchain(.toml)`, the slot cache `<workspace>/target`;
* from the GitHub Actions jobs that build the workspace (including local
  composite actions under `.github/actions`): the container image (pinned
  `rust:1.x-<distro>` becomes `rust:1-slim-<distro>`, the toolchain file
  pins the version), `apt-get install` packages (runner-only packages are
  listed as not adopted), job `services:` and PostgreSQL packages as
  `[services]` (`postgres:N`, `max_connections` when CI mentions it),
  download-and-install blocks (`curl -o FILE`, checksum, `tar`, `<tool>
  --version`) as setup steps, `taiki-e/install-action` tools with known
  recipes (cargo-nextest), literal workflow/job environment variables (CI
  cache paths are skipped), and the variables of test steps as the
  `[tool.cargo] env` allowlist;
* without such a workflow, an obvious `FROM rust:` Dockerfile;
* git dependencies on github.com in any `Cargo.toml`: `GH_TOKEN = "cmd:gh
  auth token"` in `[secrets]`, a git credential helper setup step and
  `CARGO_NET_GIT_FETCH_WITH_CLI`;
* `[resources] cpu` from the CI runner size (`8vcpu` becomes 5), local cargo
  subcommands, sccache and mold.

### doctor

`offshore doctor [--json]` checks the binary and shims, client and pool
configuration, control plane (reachability, fingerprint, API key, clock
skew), daemon, capacity, repository configuration, secrets, environment
image and FUSE; exit status 1 when a check failed
([operations.md](operations.md#status-why-and-doctor)).

### env

| command | |
|---|---|
| `offshore env [hash\|dockerfile]` | the environment image of this repository: its hash, or the generated Dockerfile |
| `offshore env build [--wait]` | build the environment image now (in the background unless `--wait`) |
| `offshore env status [--json]` | where the image exists, the last background build |

### config

| command | |
|---|---|
| `offshore config check [--pool FILE]` | validate the configuration files of this mode (client, pool when embedded, repository); `--pool` checks only a pool file |
| `offshore config show [--pool FILE]` | print the effective configuration with secrets redacted |

### prewarm and hooks

| command | |
|---|---|
| `offshore prewarm [--wait] [--force] [--quiet]` | start the remote environment of this repository in the background; `--wait` in the foreground, `--force` even if it looks warm, `--quiet` for git hooks |
| `offshore hooks install\|uninstall\|status` | git `post-checkout` hook that prewarms on new worktrees and branch checkouts |

See [operations.md](operations.md#prewarm).

## Observing

| command | |
|---|---|
| `offshore status [--json]` | machines, sessions (with labels), cost today |
| `offshore why [N\|last\|list] [--json]` | explain a recent run (N = 1 is the most recent): local or remote and why, adapter and phases, placement, timings, usage, cache counters, labels |
| `offshore top [--once] [--width N] [--window 10m] [--interval 2s]` | live view: graphs, machines, sessions, queue, cost; `--once` prints one plain frame |
| `offshore provider [list]` / `offshore provider reset NAME` | provider health (ok, cooldown, DISABLED by an account limit or auth error); re-enable a disabled provider |
| `offshore report [--since 7d] [--until T] [--by agent,worktree,branch,repo,user] [--json]` | cost, utilisation, queue wait, caches, estimator accuracy and per-label groups from the event log; times are durations (`7d`, `36h`), dates or RFC 3339 |
| `offshore metrics` | daemon, pool and machine metrics (Prometheus text) |

## Pool and daemon

| command | |
|---|---|
| `offshore daemon run\|stop\|status` | manage the local daemon (started automatically by the proxy) |
| `offshore reconcile` | reload the pool configuration and apply the policy now |
| `offshore down` | destroy all machines and caches of the pool |
| `offshore controlplane [flags]` | serve a shared control plane ([control-plane.md](control-plane.md#flags-and-commands)) |
| `offshore controlplane info [--show-key]` | URLs, TLS mode, fingerprint, API key location |
| `offshore controlplane client-config [--url U]` | print the client `[control_plane]` block |
| `offshore controlplane health` | check a running control plane (exit status) |
| `offshore version` | print the version |

## Simulator

```text
offshore sim [flags] <trace.json|trace.csv>...
    replay traces against the pool configuration and compare policies
offshore sim -sweep <trace>...
    sweep [scheduler] cost_optimization from 0 (fastest) to 1 (cheapest), print the frontier
offshore sim -generate out.json [-profile team|small] [-seed N] [-days N] [-tz ZONE]
    write a synthetic bursty workday trace
offshore sim export [-since 7d] [-until T] [-o trace.json] [-cold-penalty 0.5] [-request-cpu]
    convert the pool's event log (real runs) into a trace
```

`sim` flags: `-config` (pool file; default the active one), `-policy`
(comma separated: `default`, `binpack`, `classic`; default
`default,binpack,classic`), `-estimator static|history`, `-boot` and `-start`
(`DUR` or `provider=DUR,...`; defaults 60s and 20s), `-cold-penalty`
(default 0.5), `-queue-timeout`, `-cost-optimization 0,0.5,1` (compare
values of the default policy), `-sweep`, `-resize` (machines can grow
disks), `-json`. See
[scheduling.md](scheduling.md#simulator).

## Internal commands

Started by providers and the worker, not by users:

| command | |
|---|---|
| `offshore worker` | machine worker (configuration from `OFFSHORE_*` variables set by the provider) |
| `offshore supervisor [--spec F] [--state D]` | supervisor on VM machines (systemd, root) |
| `offshore supervisor static apply\|start\|stop\|destroy\|status` | static provider host control (over SSH) |
| `offshore sandbox-init` | session namespace init |
| `offshore gocacheprog DIR [ENDPOINT]` | `GOCACHEPROG` helper of `tool = "go"` global caches |
| `offshore debug token\|probe\|clock ...` | mint and test tokens; move the test clock (`OFFSHORE_TEST_CLOCK`) |

## Environment variables

| variable | |
|---|---|
| `OFFSHORE_DISABLE=1` | always run locally |
| `OFFSHORE_REQUIRE_REMOTE=1` | fail instead of falling back to local execution |
| `OFFSHORE_STDIN=always\|never` | stdin forwarding policy |
| `OFFSHORE_VERBOSE=1` | print the machine and cache statistics of each run |
| `OFFSHORE_NO_RETRY=1` | no second attempt when the machine is lost or kills the command (OOM, shutdown) mid-run |
| `OFFSHORE_SPLIT=0` | run split tests entirely remotely |
| `OFFSHORE_PORTS=8080,3000:3001` | forward local ports into the session (replaces `[tool.X] ports`) |
| `OFFSHORE_AGENT=NAME` | agent label of sessions (default: detected) |
| `OFFSHORE_LABELS=k=v,k=v` | extra session labels |
| `OFFSHORE_PLATFORM=linux/amd64/glibc-2.39` | override the local platform detection |
| `OFFSHORE_CONFIG=FILE` | client configuration (default `~/.config/offshore/config.toml`) |
| `OFFSHORE_POOL_CONFIG=FILE` | pool configuration (default: the client file) |
| `OFFSHORE_STATE_DIR=DIR` | daemon (or control plane) state directory |
| `OFFSHORE_SHIM_DIR=DIR` | shim directory |
| `OFFSHORE_DOCKER="sudo -n docker"` | Docker CLI of the implicit local-docker provider |
| `OFFSHORE_TOOL_CACHE=DIR` | download cache of pinned tools (sccache, mold) |
| `OFFSHORE_LOG_FORMAT=text\|json`, `OFFSHORE_LOG_LEVEL=debug\|info\|warn\|error`, `OFFSHORE_DEBUG=1` | logging of daemon, control plane and machines |
| `OFFSHORE_API_KEY`, `OFFSHORE_HOSTNAME`, `OFFSHORE_LISTEN`, `OFFSHORE_ACME_EMAIL`, `OFFSHORE_ACME_DIRECTORY`, `OFFSHORE_METRICS_LISTEN`, `OFFSHORE_MACHINE_BINARY` | control plane flag defaults |
| `OFFSHORE_MASTER_KEY`, `OFFSHORE_MASTER_KEY_FILE` | import the pool master key |
| `OTEL_*`, `OFFSHORE_OTEL_MACHINE_ENDPOINT`, `OFFSHORE_OTEL_CONSOLE_FILE` | tracing ([operations.md](operations.md#tracing)) |

Sessions see `OFFSHORE_ACTIVE=1` (recursion guard), `OFFSHORE_SESSION`
and, with `limit_jobs`, `OFFSHORE_JOBS`.
