# CLI

`cloudsourced` dispatches on its name: invoked as anything other than
`cloudsourced` (a shim such as `~/.cloudsourced/bin/cargo`) it is the proxy
for that tool. `cloudsourced help` prints the summary below.

## Running tools

| command | |
|---|---|
| `cloudsourced run [--] <tool> [args...]` | run a command through the proxy (the explicit form of a shim) |
| `cloudsourced shim install [tools...]` | install proxies into `~/.cloudsourced/bin` (`$CLOUDSOURCED_SHIM_DIR`); without tools: the `[tool.*]` sections of the current repository |
| `cloudsourced shim status` | are the proxies first in `PATH` (exit 1 if not) |
| `cloudsourced shim uninstall` | remove the proxies |
| `cloudsourced shim path` | print the shim directory |
| `cloudsourced fetch <glob>...` | copy outputs from the latest remote build of this repository (zstd) |
| `cloudsourced outputs mount\|unmount\|status` | the output overlay of the slot cache directories ([tools.md](tools.md#output-overlay)) |
| `cloudsourced editor [--mode local\|lsp-remote] vscode\|zed\|neovim` | rust-analyzer settings for remote checks ([tools.md](tools.md#rust-analyzer)) |

## Repository setup

### init

`cloudsourced init [--write [--force]]` prints a `.cloudsourced.toml` for the
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

`cloudsourced doctor [--json]` checks the binary and shims, client and pool
configuration, control plane (reachability, fingerprint, API key, clock
skew), daemon, capacity, repository configuration, secrets, environment
image and FUSE; exit status 1 when a check failed
([operations.md](operations.md#status-why-and-doctor)).

### env

| command | |
|---|---|
| `cloudsourced env [hash\|dockerfile]` | the environment image of this repository: its hash, or the generated Dockerfile |
| `cloudsourced env build [--wait]` | build the environment image now (in the background unless `--wait`) |
| `cloudsourced env status [--json]` | where the image exists, the last background build |

### config

| command | |
|---|---|
| `cloudsourced config check [--pool FILE]` | validate the configuration files of this mode (client, pool when embedded, repository); `--pool` checks only a pool file |
| `cloudsourced config show [--pool FILE]` | print the effective configuration with secrets redacted |

### prewarm and hooks

| command | |
|---|---|
| `cloudsourced prewarm [--wait] [--force] [--quiet]` | start the remote environment of this repository in the background; `--wait` in the foreground, `--force` even if it looks warm, `--quiet` for git hooks |
| `cloudsourced hooks install\|uninstall\|status` | git `post-checkout` hook that prewarms on new worktrees and branch checkouts |

See [operations.md](operations.md#prewarm).

## Observing

| command | |
|---|---|
| `cloudsourced status [--json]` | machines, sessions (with labels), cost today |
| `cloudsourced why [N\|last\|list] [--json]` | explain a recent run (N = 1 is the most recent): local or remote and why, adapter and phases, placement, timings, usage, cache counters, labels |
| `cloudsourced top [--once] [--width N] [--window 10m] [--interval 2s]` | live view: graphs, machines, sessions, queue, cost; `--once` prints one plain frame |
| `cloudsourced report [--since 7d] [--until T] [--by agent,worktree,branch,repo,user] [--json]` | cost, utilisation, queue wait, caches, estimator accuracy and per-label groups from the event log; times are durations (`7d`, `36h`), dates or RFC 3339 |
| `cloudsourced metrics` | daemon, pool and machine metrics (Prometheus text) |

## Pool and daemon

| command | |
|---|---|
| `cloudsourced daemon run\|stop\|status` | manage the local daemon (started automatically by the proxy) |
| `cloudsourced reconcile` | reload the pool configuration and apply the policy now |
| `cloudsourced down` | destroy all machines and caches of the pool |
| `cloudsourced controlplane [flags]` | serve a shared control plane ([control-plane.md](control-plane.md#flags-and-commands)) |
| `cloudsourced controlplane info [--show-key]` | URLs, TLS mode, fingerprint, API key location |
| `cloudsourced controlplane client-config [--url U]` | print the client `[control_plane]` block |
| `cloudsourced controlplane health` | check a running control plane (exit status) |
| `cloudsourced version` | print the version |

## Simulator

```text
cloudsourced sim [flags] <trace.json|trace.csv>...
    replay traces against the pool configuration and compare policies
cloudsourced sim -generate out.json [-profile team|small] [-seed N] [-days N] [-tz ZONE]
    write a synthetic bursty workday trace
cloudsourced sim export [-since 7d] [-until T] [-o trace.json] [-cold-penalty 0.5] [-request-cpu]
    convert the pool's event log (real runs) into a trace
```

`sim` flags: `-config` (pool file; default the active one), `-policy`
(comma separated: `default`, `binpack`, `classic`; default
`default,classic`), `-estimator static|history`, `-boot` and `-start`
(`DUR` or `provider=DUR,...`; defaults 60s and 20s), `-cold-penalty`
(default 0.5), `-queue-timeout`, `-json`. See
[scheduling.md](scheduling.md#simulator).

## Internal commands

Started by providers and the worker, not by users:

| command | |
|---|---|
| `cloudsourced worker` | machine worker (configuration from `CLOUDSOURCED_*` variables set by the provider) |
| `cloudsourced supervisor [--spec F] [--state D]` | supervisor on VM machines (systemd, root) |
| `cloudsourced supervisor static apply\|start\|stop\|destroy\|status` | static provider host control (over SSH) |
| `cloudsourced sandbox-init` | session namespace init |
| `cloudsourced gocacheprog DIR [ENDPOINT]` | `GOCACHEPROG` helper of `tool = "go"` global caches |
| `cloudsourced debug token\|probe\|clock ...` | mint and test tokens; move the test clock (`CLOUDSOURCED_TEST_CLOCK`) |

## Environment variables

| variable | |
|---|---|
| `CLOUDSOURCED_DISABLE=1` | always run locally |
| `CLOUDSOURCED_REQUIRE_REMOTE=1` | fail instead of falling back to local execution |
| `CLOUDSOURCED_STDIN=always\|never` | stdin forwarding policy |
| `CLOUDSOURCED_VERBOSE=1` | print the machine and cache statistics of each run |
| `CLOUDSOURCED_NO_RETRY=1` | no second attempt when the machine is lost mid-run |
| `CLOUDSOURCED_SPLIT=0` | run split tests entirely remotely |
| `CLOUDSOURCED_PORTS=8080,3000:3001` | forward local ports into the session (replaces `[tool.X] ports`) |
| `CLOUDSOURCED_AGENT=NAME` | agent label of sessions (default: detected) |
| `CLOUDSOURCED_LABELS=k=v,k=v` | extra session labels |
| `CLOUDSOURCED_PLATFORM=linux/amd64/glibc-2.39` | override the local platform detection |
| `CLOUDSOURCED_CONFIG=FILE` | client configuration (default `~/.config/cloudsourced/config.toml`) |
| `CLOUDSOURCED_POOL_CONFIG=FILE` | pool configuration (default: the client file) |
| `CLOUDSOURCED_STATE_DIR=DIR` | daemon (or control plane) state directory |
| `CLOUDSOURCED_SHIM_DIR=DIR` | shim directory |
| `CLOUDSOURCED_DOCKER="sudo -n docker"` | Docker CLI of the implicit local-docker provider |
| `CLOUDSOURCED_TOOL_CACHE=DIR` | download cache of pinned tools (sccache, mold) |
| `CLOUDSOURCED_LOG_FORMAT=text\|json`, `CLOUDSOURCED_LOG_LEVEL=debug\|info\|warn\|error`, `CLOUDSOURCED_DEBUG=1` | logging of daemon, control plane and machines |
| `CLOUDSOURCED_API_KEY`, `CLOUDSOURCED_HOSTNAME`, `CLOUDSOURCED_LISTEN`, `CLOUDSOURCED_ACME_EMAIL`, `CLOUDSOURCED_ACME_DIRECTORY`, `CLOUDSOURCED_METRICS_LISTEN`, `CLOUDSOURCED_MACHINE_BINARY` | control plane flag defaults |
| `CLOUDSOURCED_MASTER_KEY`, `CLOUDSOURCED_MASTER_KEY_FILE` | import the pool master key |
| `OTEL_*`, `CLOUDSOURCED_OTEL_MACHINE_ENDPOINT`, `CLOUDSOURCED_OTEL_CONSOLE_FILE` | tracing ([operations.md](operations.md#tracing)) |

Sessions see `CLOUDSOURCED_ACTIVE=1` (recursion guard), `CLOUDSOURCED_SESSION`
and, with `limit_jobs`, `CLOUDSOURCED_JOBS`.
