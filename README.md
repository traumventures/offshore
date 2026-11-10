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
  than the repository's build TTL (`cache.build.ttl`, default 24 h, recorded
  per slot, so repositories with different TTLs share machines). Machines
  themselves live at most `max_lifetime`, which also caps the build TTL.
* **Compiler cache.** With `[cache.compiler] tool = "sccache"` sessions run
  cargo with `RUSTC_WRAPPER=sccache` against a WebDAV cache (see below), so a
  fresh machine reuses compiled dependencies. Entries are never served after
  24 h.
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

### Configuration files

There are three configuration files, each with one role:

| file | role | read by |
|------|------|---------|
| client: `~/.config/cloudsourced/config.toml` (`$CLOUDSOURCED_CONFIG`) | `[orchestrator]` (embedded or remote), `user` | CLI, daemon |
| fleet: `[fleet]`, `[providers.*]` (`$CLOUDSOURCED_FLEET_CONFIG`, `controlplane --config`; default: the client file) | sizes, policy, lifetimes, providers | the orchestrator: daemon in embedded mode, control plane |
| repository: `.cloudsourced.toml` | environment, caches, resources, TTLs | sent with every request |

In embedded mode one file may hold the client and the fleet sections. In
remote mode fleet sections in the client file have no effect (the client warns);
the control plane ignores `[orchestrator]` and `user`. `user` (default `$USER`)
salts repository ids; `fleet.id` (default `$USER`) names the fleet and is the
`cloudsourced.owner` label of its machines and volumes. Repository settings
such as `cache.build.ttl`/`cache.deps.ttl` travel in each request, so
repositories with different settings share one fleet without affecting each
other. Fleet settings change only by reload: the daemon applies a changed fleet
file on the next request and on `cloudsourced reconcile`; the control plane
when the file changes and on `SIGHUP`. Changing `fleet.id` or the providers
needs a restart. `cloudsourced config check` validates the files of the current
mode and names their roles; `cloudsourced config show` prints the effective
configuration with secrets redacted.

```toml
user = "alice"               # client: repository id salt (default $USER)

[orchestrator]
mode = "embedded"            # client: the daemon runs the orchestrator

[fleet]                      # fleet
id             = "alice"     # labels machines (deprecated alias: owner)
machine_cpu    = 8
machine_memory = "16G"
max_machines   = 4
min_machines   = 0      # warm pool per recently used environment
idle_stop      = "20m"
max_lifetime   = "24h"

[[fleet.provision]]     # machine level provisioning, runs once per machine
run = "echo ready"

[providers.local]       # if your user cannot talk to Docker directly
backend = "local-docker"
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

The legacy form (`machine_cpu`/`machine_memory` plus `[docker]` and
`fleet.backend`) still works with a deprecation warning and maps to a single
provider named `local`; so does `fleet.owner` (now `fleet.id`; a client without
`user` keeps using it as repository id salt).

### Shared control plane

The orchestrator can run as a shared service instead of inside every daemon.
Clients only fetch leases from it; build traffic goes directly to the machines.

```sh
# fleet and providers in the control plane's fleet file (--config FILE)
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
client_cert = "client.pem"               # only with controlplane --client-ca
client_key  = "client-key.pem"
```

`--client-ca ca.pem` additionally requires a client certificate signed by that
CA (mutual TLS); the API key is still checked. The control plane keeps the
fleet master key in `<state>/master.key` (0600, created on first start). It is
the only secret that matters: every machine identity and token is derived from
it. Replicas or a rebuilt control plane share a fleet by importing the same key
with `CLOUDSOURCED_MASTER_KEY` (hex or base64) or
`CLOUDSOURCED_MASTER_KEY_FILE`.

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

Do not use root or admin credentials. `deploy/aws/iam.yaml` creates the managed
policies `cloudsourced-operator` (machines, images, artifacts, machine secrets)
and `cloudsourced-operator-stack` (resources of the shared stack) and the role
`cloudsourced-operator` with both attached:

```sh
aws cloudformation deploy --stack-name cloudsourced-iam \
  --template-file deploy/aws/iam.yaml --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides TrustedPrincipalArn=arn:aws:iam::123456789012:user/alice RequireMfa=true
```

```ini
# ~/.aws/config
[profile cloudsourced]
source_profile = alice          # your own credentials
role_arn       = arn:aws:iam::123456789012:role/cloudsourced-operator
mfa_serial     = arn:aws:iam::123456789012:mfa/alice   # with RequireMfa=true
region         = eu-central-1
```

and `profile = "cloudsourced"` in `[providers.aws]`. A control plane on EC2
trusts its instance role (`TrustedPrincipalArn=<instance role ARN>`) and
assumes the operator role, or gets the two policies attached directly
(exported as `cloudsourced-operator-policy` and
`cloudsourced-operator-stack-policy`). `CreateUser=true` adds an IAM user for
hosts that cannot assume roles. The policies only cover stacks named
`StackPrefix*` (default `cloudsourced`), instances tagged
`project=cloudsourced` launched with IMDSv2 and hop limit 1, the `cloudsourced*`
bucket and repository and SSM parameters under `/cloudsourced/`. Once the
shared stack exists, day to day operation only needs `cloudsourced-operator`.

Add `.cloudsourced.toml` to a repository:

```toml
version = 1

[resources]                 # per session, used for scheduling and cgroup limits
cpu = 4
memory = "8G"

[environment]               # built into a content addressed image
image = "rust:1-slim-bookworm"     # or: dockerfile = "ci/Dockerfile"
linker = "mold"             # optional: mold | lld (installed into the image)

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
ttl   = "24h"               # per build slot, capped by the fleet's max_lifetime

[cache.deps]
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]
ttl   = "7d"                # lifetime of a dependency volume a new machine starts

[cache.compiler]            # optional shared compiler cache
tool = "sccache"            # installs a pinned sccache into the image
ttl  = "12h"                # optional, at most 24h
# incremental = false       # CARGO_INCREMENTAL=0: workspace crates cacheable too

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
cloudsourced outputs mount          # or browse target/ of the remote build
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

<<<<<<< HEAD
### Output FUSE

By default only `eager` outputs come back after a run; `cloudsourced fetch`
copies others on request. With the output FUSE the remote build directories
(`[cache.build] paths`, e.g. `target/`) appear at their local path:

```sh
cloudsourced outputs mount      # or [outputs] mode = "fuse" in .cloudsourced.toml
cloudsourced outputs status
cloudsourced outputs unmount
```

- The mount is a read-only overlay over the local `target/` (owned by the
  daemon, mounted with `fusermount3`). Entries of the latest remote build win;
  local files that do not exist remotely (earlier local builds, fetched
  files) stay visible. Nothing below the mount point is modified, it is all
  there again after `unmount`. Local `cargo` cannot write into the mount
  (`EROFS`); unmount before building locally.
- Directories are listed on first access from the machine of the latest
  build, file contents are downloaded whole on first open into a local content
  cache (`$STATE/outputs/cas`, keyed by BLAKE3) and reused as long as path,
  size and mtime are unchanged. Every remote run starts a new generation:
  listings and kernel caches are dropped, unchanged contents are kept.
- Eager outputs inside a mounted directory are stored in the content cache
  instead of being written below the mount point, so they are served
  without the machine.
- When the machine is stopped or gone, cached listings and contents are
  still served; uncached files fail with `EIO`, uncached directories fall
  back to the local directory, and the daemon log says which machine was
  unreachable. The machine is not asked again for 20 s.
- The mount survives daemon restarts (state in `$STATE/outputs/state.json`).
=======
### Compiler cache and linker

`[cache.compiler] tool = "sccache"` adds the pinned static sccache release
(checksum verified, downloaded once on the client into the user cache
directory, `CLOUDSOURCED_TOOL_CACHE` overrides) to the environment image.
Each session gets its own sccache server on a unix socket inside the session
sandbox (`SCCACHE_SERVER_UDS`, `SCCACHE_DIR` in a private temporary
directory); it ends with the session. The cache backend is WebDAV
(`SCCACHE_WEBDAV_ENDPOINT`), served by `internal/blobcache`:

* By default the machine server serves it on `127.0.0.1` from a directory
  store under `<deps dir>/sccache` (`CLOUDSOURCED_SCCACHE_DIR`). The
  dependency volume is shared by the fleet's machines, so a new machine gets
  hits for everything an earlier one compiled. Keys are prefixed with the
  environment hash, so toolchains never mix.
* A host agent may serve the cache itself (for example from S3 with the
  instance role: `blobcache.S3StoreFromEnv`, `CLOUDSOURCED_SCCACHE_S3=bucket/cache/sccache`,
  optional `CLOUDSOURCED_SCCACHE_TTL`) and pass `CLOUDSOURCED_SCCACHE_ENDPOINT`
  to the machine server; the server appends the environment hash to that URL
  (`blobcache.Handler` without a fixed namespace).

sccache cannot cache incremental compilations, which cargo uses for
workspace crates in dev profiles; dependencies are cached regardless.
`incremental = false` sets `CARGO_INCREMENTAL=0` unless the run sets it.
User settings of `RUSTC_WRAPPER` win. With `CLOUDSOURCED_VERBOSE=1` the proxy
prints the run's statistics (`sccache: 3 hits / 1 misses (...)`); they are
also part of the session's exit message (`proto.Exit.Cache`: `sccache_requests`, `_hits`, `_misses`, `_not_cacheable`, `_errors`; shown by `cloudsourced why`).

`[environment] linker = "mold"` installs the pinned static mold release
(`lld` installs the distribution's `lld` package, Debian based images) and a
driver `/usr/local/bin/cloudsourced-cc-<linker>` (`cc ... -fuse-ld=<linker>`, last so it beats
rustc's own `-fuse-ld=lld` for its bundled rust-lld),
which sessions use as `CARGO_TARGET_<host triple>_LINKER` unless set. This
leaves `RUSTFLAGS` and `rustflags` from `.cargo/config.toml` untouched; a
`target.<triple>.linker` in `.cargo/config.toml` is overridden by it.
>>>>>>> main

### rust-analyzer

`cloudsourced editor vscode` (or `zed`, `neovim`) prints settings that run
flycheck and build scripts through the proxy. rust-analyzer stays local;
because paths are identical, diagnostics need no rewriting. Proc-macro
libraries and build script outputs must be available locally:

- `--mode local` (default): via eager outputs
  (`target/debug/deps/*.so`, `target/debug/build/*/out/**`).
- `--mode lsp-remote` (or `[editor] rust_analyzer = "lsp-remote"`): via the
  output FUSE, which the command mounts. The paths rust-analyzer gets from
  the remote `cargo check --message-format=json` (proc-macro `.so`, `OUT_DIR`)
  resolve inside the mount. `cargo metadata` stays local (dependency sources
  must exist locally). The local toolchain must match the remote one
  (`rust-toolchain.toml`), otherwise rust-analyzer's proc-macro server
  rejects the libraries.

## Development

```sh
make test                                   # unit and property tests
E2E_DOCKER="sudo -n docker" make e2e        # full end-to-end run (~5 min)
```

The end-to-end test builds a Rust project remotely and verifies outputs,
write back, exit codes, signals, stdin, environment allowlist, secrets,
services, projection consistency under rapid changes, shims, lazy outputs,
dependency caching, compiler cache hits on a fresh machine, the mold
linker, parallel sessions, scale out, idle stop, warm restart,
lifetime expiry, fail-open and cleanup.

Design notes and roadmap: [docs/PLAN.md](docs/PLAN.md).

## Security notes

* Server connections use QUIC with TLS 1.3. Each machine's Ed25519 key and
  self-signed certificate are derived from the fleet master key
  (HKDF-SHA256 over the machine id; the certificate is deterministic in id
  and expiry), so clients pin a fingerprint the orchestrator can always
  recompute and provider labels need not be trusted. A machine only receives
  the seed of its own key (`CLOUDSOURCED_TLS_SEED`, secret) and the control
  plane's public key (`CLOUDSOURCED_SIGNER_PUB`).
* Every stream carries a token signed by the control plane (Ed25519): claims
  `scope`, `machine`, `lease`, `exp`, `iat`. Sessions, root sync and output
  fetch need a `session` token, which is minted per lease, valid until the
  machine expires and only for that lease's sessions; status needs a `probe`
  token (minted per probe, two minutes); `admin` allows everything. There is
  no shared per-machine secret any more. `cloudsourced debug token|probe`
  mints and tests tokens for operators.
* Only allowlisted environment variables are forwarded; `PATH` and `HOME`
  come from the remote environment. Secrets are resolved locally per run.
* local-docker machines need `CAP_SYS_ADMIN` and `/dev/fuse` for FUSE,
  per-session mount namespaces and cgroups. Machine credentials are passed as
  container environment variables (visible to `docker inspect` on the host).
