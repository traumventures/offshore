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
                 │         │   (metadata local on the worker, content on demand by hash)
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
| client: `~/.config/cloudsourced/config.toml` (`$CLOUDSOURCED_CONFIG`) | `[control_plane]` (absent: embedded orchestrator), `user` | CLI, daemon |
| pool: `[pool]`, `[providers.*]` (`$CLOUDSOURCED_POOL_CONFIG`, `controlplane --config`; default: the client file) | sizes, policy, lifetimes, providers | the orchestrator: daemon in embedded mode, control plane |
| repository: `.cloudsourced.toml` | environment, caches, resources, TTLs | sent with every request |

In embedded mode one file may hold the client and the pool sections. In
remote mode pool sections in the client file have no effect (the client warns);
the control plane ignores `[control_plane]` and `user`. `user` (default `$USER`)
salts repository ids; `pool.id` (default `$USER`) names the pool and is the
`cloudsourced.pool` label of its machines and volumes. Repository settings
such as `cache.build.ttl`/`cache.deps.ttl` travel in each request, so
repositories with different settings share one pool without affecting each
other. Pool settings change only by reload: the daemon applies a changed pool
file on the next request and on `cloudsourced reconcile`; the control plane
when the file changes and on `SIGHUP`. Changing `pool.id` or the providers
needs a restart. `cloudsourced config check` validates the files of the current
mode and names their roles; `cloudsourced config show` prints the effective
configuration with secrets redacted.

```toml
user = "alice"               # client: repository id salt (default $USER)

[pool]                       # pool
id             = "alice"     # labels machines (cloudsourced.pool)
machine_cpu    = 8
machine_memory = "16G"
max_machines   = 4
min_machines   = 0      # warm pool per recently used environment
idle_stop      = "20m"
max_lifetime   = "24h"

[[machine.setup]]     # machine level provisioning, runs once per machine
run = "echo ready"

[providers.local]       # if your user cannot talk to Docker directly
type = "local-docker"
command = ["sudo", "-n", "docker"]
```

### Scheduling, overcommit and multiple providers

Placement and scaling are decided by a pure policy (`internal/scheduler`,
interfaces `Policy` and `Estimator`); the orchestrator only executes its
actions. One orchestrator can span several providers:

```toml
[providers.local]
type         = "local-docker"
command      = ["sudo", "-n", "docker"]
max_machines = 4

[scheduler]
policy            = "default"
estimator         = "history"  # learn per repo/tool/subcommand from cgroup usage, or "static"
cpu_overcommit    = 2.0        # only slows builds down
memory_overcommit = 1.25       # requires soft_limit or swap, otherwise OOM
soft_limit        = 1.2        # memory.high = request * 1.2
swap              = true
max_memory_pressure = 0.4      # PSI; no new sessions above this
limit_jobs        = true       # CARGO_BUILD_JOBS = granted cores

[[providers.local.size]]
name     = "small"
cpu      = 4
memory   = "8G"
price_per_hour = 0.1
```

Without `[providers]` the pool has one local-docker provider `local`
(command `$CLOUDSOURCED_DOCKER`, default `docker`); a provider without
`[[providers.NAME.size]]` gets one size `default` of `pool.machine_cpu` and
`pool.machine_memory`.

### Shared control plane

The orchestrator can run as a shared service instead of inside every daemon.
Clients only fetch leases from it; build traffic goes directly to the machines.

```sh
# pool and providers in the control plane's pool file (--config FILE)
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
[control_plane]
url         = "https://10.0.0.5:7443"   # or https://cs.example.com
fingerprint = "0b91…"                    # only for the self-signed certificate
api_key     = "env:CLOUDSOURCED_API_KEY" # or file:…, cmd:…
client_cert = "client.pem"               # only with controlplane --client-ca
client_key  = "client-key.pem"
```

`--client-ca ca.pem` additionally requires a client certificate signed by that
CA (mutual TLS); the API key is still checked. The control plane keeps the
pool master key in `<state>/master.key` (0600, created on first start). It is
the only secret that matters: every machine identity and token is derived from
it. Replicas or a rebuilt control plane share a pool by importing the same key
with `CLOUDSOURCED_MASTER_KEY` (hex or base64) or
`CLOUDSOURCED_MASTER_KEY_FILE`.

Container image (`make image`), running it on a Docker host and on AWS
(`deploy/aws/controlplane.yaml`): [docs/CONTROLPLANE.md](docs/CONTROLPLANE.md).

### AWS EC2

```toml
[providers.aws]
type           = "aws-ec2"
profile        = "my-profile"           # optional
region         = "eu-central-1"
allow_cidrs    = ["203.0.113.4/32"]     # who may reach machines (QUIC, UDP 7443); default: own public IP
docker_command = ["docker"]             # builds and pushes environment images
disk_gb        = 100

[[providers.aws.size]]
name     = "c7a.2xlarge"     # instance type
cpu      = 8
memory   = "15G"
price_per_hour = 0.42
```

On first use the provider creates the CloudFormation stack `cloudsourced`
(`internal/provider/awsec2/stack.yaml`): security group, instance role, S3
bucket for the worker binary, ECR repository for environment images, a reaper
Lambda (every 10 minutes it terminates machines past their expiry) and a
resource group of everything tagged `project=cloudsourced`. Machines are
Amazon Linux 2023 instances running the same container layout as local-docker
(`internal/machine`), applied by the supervisor `cloudsourced supervisor` (a systemd
unit). User data carries only the public machine spec; the machine's secrets
(server key, token) travel in an SSM SecureString parameter
`/cloudsourced/<stack>/machines/<id>` that the supervisor reads once with the
instance role and deletes. Machines terminate themselves at `max_lifetime`,
and stop themselves when idle for `idle_stop` while no orchestrator has probed
them for `max(idle_stop, 10m)` (an offline control plane does not leave them
running). The metadata service is not reachable from sessions (IMDSv2, hop
limit 1). The caller's credentials need `ssm:PutParameter`/`DeleteParameter`.

#### Instance types and arm64

```toml
[[providers.aws.size]]
name           = "x4"                 # a name of your choice
cpu            = 4
memory         = "7G"
price_per_hour = 0.21
instance_types = ["c7a.xlarge", "c6a.xlarge", "m7a.xlarge"]   # tried in order

[[providers.aws.size]]
name           = "c7g.xlarge"
arch           = "arm64"              # default "amd64"
cpu            = 4
memory         = "7G"
price_per_hour = 0.17
```

All machines are on-demand instances (spot capacity is not supported). The
instance types of a size are tried in order (default: the size name); any
capacity error (`InsufficientInstanceCapacity`, vCPU quota, type not offered
in the zone) moves on to the next type.

`arch = "arm64"` sizes (Graviton: c7g, m7g, t4g, ...) boot the arm64 Amazon
Linux 2023 AMI and run `bin/cloudsourced-linux-arm64`, which `make build`
puts next to `bin/cloudsourced` (provider options `binary_amd64`,
`binary_arm64` and `ami_arm64` override). Environments are per architecture:
a repository pins `resources.arch = "aarch64"` (default `"x86_64"`, because
build outputs must run on the client) and only sizes of that architecture
are created for it; its environment image is built for `linux/arm64`, which
needs either a native arm64 builder or, on an amd64 control plane, Docker with
BuildKit and QEMU binfmt handlers (`docker run --privileged --rm
tonistiigi/binfmt:qemu-v9.2.2 --install arm64`; emulated builds are several
times slower).

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
ttl   = "24h"               # per build slot, capped by the pool's max_lifetime

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

### Oracle Cloud Infrastructure (OCI)

```toml
[providers.oci]
type        = "oci"
profile     = "DEFAULT"            # ~/.oci/config profile (config_file overrides the path)
region      = "eu-frankfurt-1"     # default: the profile's region
compartment = "cloudsourced"       # name below the tenancy root (created) or an OCID
allow_cidrs = ["203.0.113.4/32"]   # who may reach machines (QUIC, UDP 7443); default: own public IP
disk_gb     = 100                  # boot volume (min 50)
docker_command = ["docker"]        # builds and pushes environment images

[[providers.oci.size]]
name           = "e5-4"
cpu            = 4                 # vCPUs: 2 OCPUs on x86 shapes, 4 on Ampere A1
memory         = "16G"
price_per_hour = 0.11
instance_types = ["VM.Standard.E5.Flex"]   # shapes, tried in order (default: E5.Flex, A1.Flex for arm64)

[[providers.oci.size]]
name           = "a1-4"
arch           = "arm64"
cpu            = 4
memory         = "24G"
price_per_hour = 0.07
```

On first use the provider creates, idempotently: the compartment, a VCN
`cloudsourced` (10.77.0.0/16) with internet gateway, default route and one
public subnet, whose security list admits only UDP 7443 from `allow_cidrs`
(no SSH); a private Object Storage bucket `cloudsourced` (server binaries,
machine secrets, compiler cache below `cache/`, expired after a day by a
lifecycle rule); a container repository `cloudsourced-env` in OCIR; and a
dynamic group of the compartment's instances with a policy that lets them
stop and terminate instances of the compartment, use the bucket and pull
images (`iam = false` skips the IAM part when the API user may not manage
it; an administrator then creates the dynamic group and the policy from
`policyStatements` in `internal/provider/oci/infra.go`). Options `subnet`,
`availability_domain`, `image`/`image_arm64`, `bucket`, `repository` and
`registry` override the defaults.

Machines boot the newest Oracle Linux 9 platform image, install Docker CE and
run the same supervisor and container layout as on AWS. Flexible shapes are
sized from the size's `cpu` (one OCPU is two vCPUs on x86, one core on A1)
and `memory`. The machine's secrets travel in the object `secrets/<id>.json`,
which the supervisor reads once with its instance principal and deletes;
user data holds only the public spec and a read-only pre-authenticated link
to the server binary. Containers cannot reach the metadata service (the
supervisor unit rejects forwarded traffic to 169.254.0.0/16). Images are
pushed with a registry token of the API key (`docker login -u BEARER_TOKEN`),
and machines pull with a token of their instance principal.

Differences to AWS: an OS shutdown only stops an OCI instance, so at
`max_lifetime` the supervisor terminates its instance through the API; there
is no reaper function. Preemptible capacity is not supported. Shapes are
tried in order; "Out of host capacity", service limit and unavailable-shape
errors move on to the next shape. A live smoke
test is in `internal/orchestrator/ocismoke_test.go` (build tag `ocismoke`).

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

### Compiler cache and linker

`[cache.compiler] tool = "sccache"` adds the pinned static sccache release
(checksum verified, downloaded once on the client into the user cache
directory, `CLOUDSOURCED_TOOL_CACHE` overrides) to the environment image.
Each session gets its own sccache server on a unix socket inside the session
sandbox (`SCCACHE_SERVER_UDS`, `SCCACHE_DIR` in a private temporary
directory); it ends with the session. The cache backend is WebDAV
(`SCCACHE_WEBDAV_ENDPOINT`), served by `internal/blobcache`:

* By default the worker serves it on `127.0.0.1` from a directory
  store under `<deps dir>/sccache` (`CLOUDSOURCED_SCCACHE_DIR`). The
  dependency volume is shared by the pool's machines, so a new machine gets
  hits for everything an earlier one compiled. Keys are prefixed with the
  environment hash, so toolchains never mix.
* On AWS the supervisor serves the cache itself on the Docker bridge
  gateway (port 7480, reachable from sessions, not from outside the host)
  from the stack's bucket with the instance role (`s3://<bucket>/cache/<owner>/sccache/`,
  deleted after a day by the bucket lifecycle rule), and passes
  `CLOUDSOURCED_SCCACHE_ENDPOINT` to the worker; the worker appends
  the environment hash to that URL (`blobcache.Handler` without a fixed
  namespace). `blobcache.S3StoreFromEnv` (`CLOUDSOURCED_SCCACHE_S3=bucket/prefix`)
  offers the same store to other hosts.

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

## Compatibility

Machines are Linux x86_64 (arm64 machines may follow). Everything that
builds or tests through `cargo` (or another proxied tool) on Linux works;
what needs local hardware, a GUI or another OS stays local.

**Works remotely** (output identical to a local run):

* `cargo build`, `check`, `test`, `clippy`, `doc`, `bench`, `run` (also with
  forwarded ports, see `[tool.X] ports`), `fmt`
* `cargo nextest` (the whole run executes on one machine; running nextest
  locally is fine too), `cargo hack`, `cargo llvm-cov`, `cargo miri`,
  `cargo fuzz`, `cargo udeps`, `cargo expand`, `cargo deny`, `cargo audit`,
  `cargo insta`
* cross compilation with target toolchains: `cargo zigbuild`, `cargo xwin`,
  `wasm-pack build`, `trunk build`, `maturin build`
* linkers `mold` and `lld` (`[environment] linker`)
* wrappers such as `bacon`, `cargo watch`, `just` and `make` run locally;
  the `cargo` they start goes through the proxy

**Limited:**

* rust-analyzer stays local; flycheck and build scripts run remotely
  (`cloudsourced editor`). Proc-macro dylibs must come back as outputs
  (eager outputs or the output mount); a fully remote mode via an LSP
  bridge is not part of this release.
* `cargo tarpaulin` needs `ptrace`, which sessions do not get; use
  `cargo llvm-cov`.
* `maturin develop` installs into the local virtualenv: build remotely with
  `maturin build`, install the wheel locally.
* `cross` needs Docker in the session (not available): use `zigbuild` or
  target toolchains in the environment image.
* Bazel and Buck2 have their own remote execution; use that instead.

**Local only:**

* flashing and debugging hardware (`probe-rs`, `espflash`, `cargo embed`):
  build remotely, flash locally (`cloudsourced fetch` or outputs).
* `trunk serve`, `tauri dev` and other dev servers can run remotely with
  port forwarding (`[tool.X] ports`); anything that opens local windows or
  devices runs locally.
* macOS, iOS and Windows-native targets (other than `xwin` builds) need the
  respective host and run locally.

Only tools with an installed shim are proxied; `CLOUDSOURCED_DISABLE=1`
always runs locally.

## Operations

**Metrics.** All components export Prometheus text format (no extra
dependencies; `internal/metrics`):

* Control plane: `GET /metrics` on the API port (API key required) and,
  with `--metrics-listen 127.0.0.1:9090` (`$CLOUDSOURCED_METRICS_LISTEN`),
  without authentication on a separate plain HTTP listener for a local
  scraper. Pool metrics: `cloudsourced_machines{provider,size,state}`,
  `cloudsourced_queue_length`, `cloudsourced_acquire_wait_seconds{result}`,
  `cloudsourced_placements_total{kind}` (assign, queue, reject, timeout),
  `cloudsourced_pool_actions_total{action}`,
  `cloudsourced_provider_errors_total{provider,op}`,
  `cloudsourced_image_builds_total{provider,result}`,
  `cloudsourced_image_build_seconds{provider}`,
  `cloudsourced_reconcile_duration_seconds{result}`,
  `cloudsourced_cost_per_hour{provider}`, `cloudsourced_environments_recent`,
  `cloudsourced_http_requests_total{path,result}`,
  `cloudsourced_http_request_duration_seconds{path}`.
* Machines report their metrics in the status stream the orchestrator
  already probes (scope `probe`, no extra port), and the orchestrator
  republishes them with `machine`, `provider` and `size` labels:
  `cloudsourced_worker_sessions_running`,
  `cloudsourced_worker_session_duration_seconds`,
  `cloudsourced_worker_session_exits_total{code}`,
  `cloudsourced_worker_oom_kills_total`,
  `cloudsourced_worker_sessions_rejected_total{reason}`,
  `cloudsourced_worker_pressure{resource}`,
  `cloudsourced_worker_reserved{resource}`,
  `cloudsourced_worker_memory_used_bytes`,
  `cloudsourced_worker_compiler_cache_total{counter}` (sccache counters),
  `cloudsourced_worker_transfer_bytes_total{direction}` (fetch, writeback,
  output), `cloudsourced_worker_ready`, `_draining`, `_warm_repos`. Machine
  counters restart with the worker.
* `cloudsourced metrics` prints the daemon's metrics
  (`cloudsourced_daemon_requests_total{op,result}`,
  `cloudsourced_daemon_prepare_seconds{result}`,
  `cloudsourced_daemon_runs_total{tool,mode}`, `cloudsourced_daemon_roots`,
  `cloudsourced_daemon_machine_connections`) with the embedded
  orchestrator's (embedded mode) or the control plane's (remote mode).

**Logs.** `CLOUDSOURCED_LOG_FORMAT=json` (default `text`) and
`CLOUDSOURCED_LOG_LEVEL=debug|info|warn|error` apply to the daemon
(`<state>/daemon.log`), the control plane and workers; the
orchestrator passes both to the machines it creates. The control plane logs
every API request with a `request_id` (the client's `X-Request-Id` when
well formed, returned in the response header), and acquisitions with
lease, machine and placement reason.

**Tracing.** OpenTelemetry tracing is off by default. The standard
variables enable it in every component (`OTEL_TRACES_EXPORTER=otlp|console|none`,
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER[_ARG]`,
`OTEL_SDK_DISABLED`), as does a `[telemetry]` section in the client and the
pool configuration (the environment wins):

```toml
[telemetry]
endpoint = "https://otel.example.com:4318"  # OTLP/HTTP base URL
headers = { "x-api-key" = "env:OTEL_KEY" }  # references only (env:, file:, cmd:)
sampling = 0.25                             # new traces; downstream follows the parent
machine_endpoint = "http://10.0.0.5:4318"   # pool file: the collector as seen by machines
```

Spans are exported as OTLP/HTTP with JSON encoding (accepted by the
OpenTelemetry Collector and OTLP backends on `/v1/traces`; OTLP/gRPC is not
supported, which keeps gRPC and protobuf out of the static binary). Default
service names: `cloudsourced-proxy`, `-daemon`, `-controlplane`, `-server`,
`-host`. The orchestrator forwards the exporter settings to the machines it
creates (headers as secrets; `machine_endpoint` or
`$CLOUDSOURCED_OTEL_MACHINE_ENDPOINT` overrides the endpoint), servers do
not pass `OTEL_*` on to sessions, and `console` writes to stderr or to
`$CLOUDSOURCED_OTEL_CONSOLE_FILE`. One tool run is one trace, propagated as
W3C `traceparent` (daemon RPC field, control plane HTTP header, optional
`traceparent` field of the QUIC `Hello` and `Exec` frames):

```
cloudsourced.run (proxy: tool, subcommand, flag names, repo id, decision, reason, exit code)
├─ proxy.prepare
│  └─ daemon.prepare (repo id, env hash, usage key, estimate, lease, machine)
│     ├─ daemon.env_resolve, daemon.image_ensure, daemon.manifest_refresh, daemon.estimate
│     ├─ [remote] controlplane /v1/acquire (client) > controlplane /v1/acquire (server)
│     │  └─ pool.acquire (queue wait, placement, reason, machine, provider, size, price, reservation)
│     │       ~ pool.create / pool.start (linked root spans: provider, size, price, duration, ready event)
│     └─ daemon.attach (machine, attempt)
├─ proxy.connect (session, worker wait)
├─ worker.exec (machine: lease, reservation, slot, warm, peak memory, CPU seconds, OOM, exit code, sccache counters)
│  ├─ worker.process, worker.writeback, worker.outputs
├─ proxy.exec (exit code, usage), proxy.writeback, proxy.outputs
└─ daemon.release > controlplane /v1/release
orchestrator.reconcile > pool.stop / pool.destroy (background, provider latency)
```

Only the subcommand and the names of flags of a tool's arguments are
recorded, never their values, positional arguments, environment or tokens.

**Event log and reports.** Independent of tracing, the orchestrator writes
an analysis event log (`events-YYYY-MM-DD.jsonl` in its state directory,
`[telemetry] events_retention_days`, default 30) of task and machine events.
`cloudsourced report [--since 7d] [--json]` shows cost per day, repository
and size, utilisation and overprovisioning, queue wait p50/p95, warm vs.
cold caches and estimator accuracy; `cloudsourced sim export --since 7d -o
trace.json` converts the real runs into a simulator trace (both work
embedded and remote, via the control plane's `POST /v1/events`, API key
protected). See [docs/SCHEDULER.md](docs/SCHEDULER.md#calibrating-the-simulator-from-real-runs).

**24 h guarantees and cleanup.**

| What | Bound | Enforced by |
| --- | --- | --- |
| machines | `max_lifetime` (default 24h) | server drains and exits at expiry; orchestrator destroys; AWS: instance shuts down (terminate on shutdown), reaper Lambda terminates 5 min after `expires-at` every 10 min |
| build slots | build TTL ≤ 24h ≤ `max_lifetime` | server garbage collection; die with the machine's cache volume |
| compiler cache | 24h | blobcache never serves older entries; S3 lifecycle `cache/` 1 day |
| dependency caches | `[cache.deps] ttl` (default 7 days) | volume expiry label, orchestrator reaps volumes |
| machine secrets (AWS SSM) | until the supervisor reads them | agent deletes after reading; reaper deletes parameters under `/cloudsourced/<stack>/machines/` without a live machine (after 15 min) |
| server binaries (S3 `bin/`) | 30 days | bucket lifecycle |
| environment images (ECR) | 30 days | repository lifecycle |

## Development

```sh
make test                                   # unit and property tests
E2E_DOCKER="sudo -n docker" make e2e        # full end-to-end run
E2E_PARALLEL=1 E2E_DOCKER="sudo -n docker" ./test/e2e/run.sh   # parts concurrently
E2E_REMOTE=1 E2E_DOCKER="sudo -n docker" ./test/e2e/run.sh     # against a control plane
```

The test is split into parts (`test/e2e/parts`: `core` without compiling,
`rust`, `lifecycle`), selectable with `E2E_PARTS`; each part has its own
owner, daemon, ports and test clock. Idle stop and lifetime expiry use the
test clock (`CLOUDSOURCED_TEST_CLOCK=offset:FILE`, moved with the hidden
`cloudsourced debug clock advance DURATION`) and a fast reconcile loop
(`CLOUDSOURCED_RECONCILE_INTERVAL`), so they take seconds. In remote mode
only `core` runs unless `E2E_FULL=1`. The environment image is reused
across runs (`E2E_CLEAN_IMAGES=1` removes the images a run built).

The end-to-end test builds a Rust project remotely and verifies outputs,
write back, exit codes, signals, stdin, environment allowlist, secrets,
services, projection consistency under rapid changes, shims, lazy outputs,
dependency caching, compiler cache hits on a fresh machine, the mold
linker, parallel sessions, scale out, idle stop, warm restart,
lifetime expiry, fail-open and cleanup.

Design notes and roadmap: [docs/PLAN.md](docs/PLAN.md).

## Security notes

* Server connections use QUIC with TLS 1.3. Each machine's Ed25519 key and
  self-signed certificate are derived from the pool master key
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
