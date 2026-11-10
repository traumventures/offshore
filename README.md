# cloudsourced

Run build and test commands on cloud machines as if they ran locally.

```console
$ cargo build          # ~/.cloudsourced/bin/cargo -> cloudsourced
   Compiling hello v0.1.0 (/home/me/hello)
    Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.61s
$ ./target/debug/hello # the binary was copied back
```

`cloudsourced` is a single static Go binary with three roles:

| Role | Runs on | Does |
|---|---|---|
| proxy | laptop | Invoked as `cargo` (or any configured tool) through a symlink. Decides local or remote, then behaves exactly like the local process: stdout/stderr, terminal, stdin, signals, exit code. |
| daemon | laptop | Watches sources, keeps versioned manifests, talks to machines, builds environment images and runs the embedded orchestrator (placement, scale out, idle stop, lifetime rotation, cleanup). |
| server | machine | Projects the client's sources over FUSE at the **same absolute path**, runs sessions in their own mount namespace and cgroup, keeps warm build caches (max 24 h), writes changed sources back and returns outputs. |

The first backend is **local-docker**: machines are Docker containers on the
local host. Everything else is real (FUSE, sessions, caches, provisioning,
orchestration), so the whole system can be developed and tested without a
cloud account. Cloud backends implement the same `provider.Provider`
interface.

## How it works

```text
laptop                                         machine (container / VM)
cargo build ──► proxy ──► daemon ── QUIC ────► server
                 │         ├─ watcher + manifest ──► FUSE projection of /home/me/hello
                 │         │   (metadata local on the server, content on demand by hash)
                 │         └─ orchestrator ─────► provider: create/start/stop/destroy
                 └──────────── QUIC exec ───────► session: own mount ns + cgroup
                                                    /home/me/hello/target = warm slot
                 ◄── stdout/stderr/exit ───────
                 ◄── write back (Cargo.lock …) ─
                 ◄── eager outputs (binaries) ──
```

* **Barrier.** Every run carries the manifest version of each root. The
  server starts the process only after it applied that version, so the
  remote side never sees stale files. Before each run the daemon does a stat
  walk (hashing only changed files); the watcher pushes changes earlier.
* **Projection.** `lookup`/`getattr`/`readdir` are answered from the manifest
  without a network round trip; kernel caches are kept for an hour and
  invalidated precisely when deltas arrive. Content is fetched once per hash
  into a content addressed store.
* **Writes** to sources (e.g. `Cargo.lock`, `cargo fmt`) go to an overlay and
  are written back after the run, unless the local file changed meanwhile.
* **Build caches.** Worker-local directories (`target/`) are bind-mounted from
  build slots. A session leases a slot exclusively, so parallel builds of one
  repository never block each other; slots are reused warm and never older
  than the build TTL (24 h). Machines themselves live at most `max_lifetime`.
* **Placement** prefers machines that already hold a warm slot for the
  repository, then rendezvous hashing; memory is never overcommitted, CPU is
  (`cpu_overcommit`). Several sessions share one machine; more machines are
  created up to `max_machines`; idle machines stop (disk kept), expired ones
  are destroyed with their volumes.

## Quick start (local-docker)

Requirements: Linux, Docker, `/dev/fuse`, Go ≥ 1.24.

```sh
make build                       # static binary in bin/cloudsourced
bin/cloudsourced shim install cargo
export PATH="$HOME/.cloudsourced/bin:$PATH"
```

If your user cannot talk to Docker directly, configure the CLI invocation in
`~/.config/cloudsourced/config.toml`:

```toml
[fleet]
machine_cpu    = 8
machine_memory = "16G"
max_machines   = 4
min_machines   = 0      # warm pool per recently used environment
idle_stop      = "20m"
max_lifetime   = "24h"

[[fleet.provision]]     # machine level provisioning, runs once per machine
run = "echo ready"

[docker]
command = ["sudo", "-n", "docker"]
```

### Scheduling, overcommit and multiple providers

Placement and scaling are decided by a pure policy (`internal/scheduler`,
interfaces `Policy` and `Estimator`); the orchestrator only executes its
actions. One orchestrator can span several providers:

```toml
[fleet]
policy            = "default"
estimator         = "history"  # learn per repo/tool/subcommand from cgroup usage, or "static"
cpu_overcommit    = 2.0        # only slows builds down
memory_overcommit = 1.25       # requires soft_limit or swap, otherwise OOM
soft_limit        = 1.2        # memory.high = request * 1.2
swap              = true
max_memory_pressure = 0.4      # PSI; no new sessions above this
limit_jobs        = true       # CARGO_BUILD_JOBS = granted cores

[[fleet.size]]
provider = "local"
name     = "small"
cpu      = 4
memory   = "8G"
price_per_hour = 0.1

[providers.local]
backend      = "local-docker"
command      = ["sudo", "-n", "docker"]
max_machines = 4
```

The legacy form (`machine_cpu`/`machine_memory` plus `[docker]`) still works and
maps to a single provider named `local`.

### Shared control plane

The orchestrator can run as a shared service instead of inside every daemon.
Clients only fetch leases from it; build traffic goes directly to the machines.

```sh
# fleet and providers in the control plane's config.toml
cloudsourced controlplane                        # self-signed, :7443, fingerprint pinned
cloudsourced controlplane --hostname cs.example.com   # Let's Encrypt, :443 (+ :80 for ACME)
cloudsourced controlplane client-config          # prints the client block below
cloudsourced controlplane info [--show-key]      # URLs, fingerprint, API key location
```

The API key is generated once (`<state>/api-key`) and never logged. Without a
domain the control plane pins its persistent self-signed certificate; with one,
`--hostname` is all it takes (or pass `--tls-cert/--tls-key`, or terminate TLS
in a reverse proxy and use `--insecure-http`). Clients:

```toml
[orchestrator]
mode        = "remote"
url         = "https://10.0.0.5:7443"   # or https://cs.example.com
fingerprint = "0b91…"                    # only for the self-signed certificate
api_key     = "env:CLOUDSOURCED_API_KEY" # or file:…, cmd:…
```

Container image (`make image`), running it on a Docker host and on AWS
(`deploy/aws/controlplane.yaml`): [docs/CONTROLPLANE.md](docs/CONTROLPLANE.md).

### AWS EC2

```toml
[[fleet.size]]
provider = "aws"
name     = "c7a.2xlarge"     # instance type
cpu      = 8
memory   = "15G"
price_per_hour = 0.42

[providers.aws]
backend        = "aws-ec2"
profile        = "my-profile"           # optional
region         = "eu-central-1"
allow_cidrs    = ["203.0.113.4/32"]     # who may reach machines (QUIC, UDP 7443); default: own public IP
docker_command = ["docker"]             # builds and pushes environment images
disk_gb        = 100
```

On first use the provider creates the CloudFormation stack `cloudsourced`
(`internal/provider/awsec2/stack.yaml`): security group, instance role, S3
bucket for the server binary, ECR repository for environment images, a reaper
Lambda (every 10 minutes it terminates machines past their expiry) and a
resource group of everything tagged `project=cloudsourced`. Machines are
Amazon Linux 2023 instances running the same container layout as local-docker;
they terminate themselves at `max_lifetime`, and the metadata service is not
reachable from sessions (IMDSv2, hop limit 1).

Add `.cloudsourced.toml` to a repository:

```toml
version = 1

[resources]                 # per session, used for scheduling and cgroup limits
cpu = 4
memory = "8G"

[environment]               # built into a content addressed image
image = "rust:1-slim-bookworm"     # or: dockerfile = "ci/Dockerfile"

[environment.packages]
apt = ["pkg-config", "libssl-dev", "clang"]

[environment.toolchains]
rust = "rust-toolchain.toml"

[[environment.setup]]       # environment provisioning, cached per hash
run = "cargo install --locked cargo-nextest"

[[source.rule]]
paths = ["vendor/**"]
mode  = "immutable"         # watch (default) | immutable | hidden

[cache.build]
paths = ["target"]          # worker-local, never synchronised
ttl   = "24h"

[cache.deps]
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]
ttl   = "7d"

[services.postgres]         # sidecar, reachable on localhost in sessions
image = "postgres:17"
env   = { POSTGRES_PASSWORD = "dev" }
port  = 5432

[secrets]                   # resolved locally per run, never stored remotely
NPM_TOKEN = "cmd:agentpass get npm/token"   # or env:VAR, file:PATH

[tool.cargo]
local = ["publish", "login", "install", "new", "init", "add", "remove"]
eager = ["target/debug/myapp"]              # copied back after every run
env   = ["RUSTFLAGS", "CARGO_TERM_*", "RUST_LOG", "RUST_BACKTRACE"]

[tool.npm]
ports = [8080, "3000:3001"]  # forward 127.0.0.1:local -> session port while it runs
```

Then just use the tool:

```sh
cargo build
cargo test
cloudsourced status                 # machines, sessions, cost today (--json)
cloudsourced why                    # last run: local/remote and why, machine,
                                    # placement, timings, usage (N, list, --json)
cloudsourced fetch 'target/debug/deps/*.so'   # lazy outputs
cloudsourced down                   # destroy machines and caches
```

Escape hatches: `CLOUDSOURCED_DISABLE=1` (always local),
`CLOUDSOURCED_REQUIRE_REMOTE=1` (fail instead of falling back),
`CLOUDSOURCED_STDIN=always|never`, `CLOUDSOURCED_VERBOSE=1`.
When no machine can be provisioned the proxy falls back to the local tool.

Port forwarding: while a remote session runs, the proxy listens on
`127.0.0.1:<local>` for every port of `[tool.X] ports` (or
`CLOUDSOURCED_PORTS=8080,3000:3001`, which replaces the configured list)
and carries each connection over its own QUIC stream to
`127.0.0.1:<remote>` in the machine's network namespace, which sessions and
service containers share. Connections end with the session. A busy local
port is reported and skipped; the command still runs.

`cloudsourced why` reads the last 50 runs from `<state>/runs.jsonl`. The
`cache` field carries counters of cache integrations (e.g. sccache hits).
`cloudsourced status` estimates today's cost (since local midnight) per
machine size as `price_per_hour` times the uptime the orchestrator observed
(starting or running). Machines first seen up count from their creation
time; restarts and removals that happened while the daemon or control plane
was not running are not visible, and provider billing granularity is
ignored.

### rust-analyzer

`cloudsourced editor vscode` (or `zed`, `neovim`) prints settings that run
flycheck and build scripts through the proxy. rust-analyzer stays local;
because paths are identical, diagnostics need no rewriting. Proc-macro
libraries and build script outputs are returned via eager outputs
(`target/debug/deps/*.so`, `target/debug/build/*/out/**`).

## Development

```sh
make test                                   # unit and property tests
E2E_DOCKER="sudo -n docker" make e2e        # full end-to-end run (~5 min)
```

The end-to-end test builds a Rust project remotely and verifies outputs,
write back, exit codes, signals, stdin, environment allowlist, secrets,
services, projection consistency under rapid changes, shims, lazy outputs,
dependency caching, parallel sessions, scale out, idle stop, warm restart,
lifetime expiry, fail-open and cleanup.

Design notes and roadmap: [docs/PLAN.md](docs/PLAN.md).

## Security notes

* Server connections use QUIC with TLS 1.3; the client pins the
  self-signed certificate generated per machine, and every stream carries a
  per-machine token.
* Only allowlisted environment variables are forwarded; `PATH` and `HOME`
  come from the remote environment. Secrets are resolved locally per run.
* local-docker machines need `CAP_SYS_ADMIN` and `/dev/fuse` for FUSE,
  per-session mount namespaces and cgroups. Machine credentials are passed as
  container environment variables (visible to `docker inspect` on the host).
