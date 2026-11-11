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
* **Caches** (`[cache.<name>]`, see [Caches](#caches)) have a scope:
  `slot` caches (`target/`) are worker-local directories bind-mounted from
  build slots. A session leases a slot exclusively, so parallel builds of one
  repository never block each other; slots are reused warm and never older
  than the cache's `ttl` (default 24 h, recorded per slot, so repositories
  with different TTLs share machines). `machine` caches (cargo registry,
  `node_modules` per lockfile digest) are shared by the sessions of a
  machine, `global` caches (sccache, ccache, Go) by all machines of the
  pool; their entries are never served after 24 h. Machines themselves live
  at most `max_lifetime`, which also caps the slot TTL.
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
export PATH="$HOME/.cloudsourced/bin:$PATH"   # in ~/.profile or ~/.zshrc
bin/cloudsourced shim status     # checks that the shims are first in PATH
```

Shims are the standard setup: with `~/.cloudsourced/bin` first in PATH,
plain `cargo build`, `cargo test` and `cargo nextest run` in a repository
with a `.cloudsourced.toml` go through cloudsourced, for people, scripts and
coding agents alike. `cloudsourced shim install` without tools installs the
tools of the current repository's `[tool.*]` sections; `cloudsourced shim
status` exits 1 when a shim is missing from PATH or shadowed by an earlier
entry. `cloudsourced run <tool> ...` is the explicit form without shims.

### Getting a repository ready: `init`, `doctor`

`cloudsourced init` prints a `.cloudsourced.toml` for the current
repository (git top level) and `cloudsourced init --write` writes it
(`--force` replaces an existing one). It is derived from what the repository
already declares, with a comment explaining every detected item:

* the ecosystems (tool adapters: cargo, go via `go.mod`/`go.work`, node via
  `package.json`) and their default caches as `[cache.<name>]` (see "Tool
  adapters"), plus `[tool.go]`/`[tool.<package manager>]` sections and a
  `golang:<go directive>` or bun/node image when there is no Rust;
* the Cargo workspace (repository root or a subdirectory such as `app/`),
  its `rust-toolchain(.toml)`, the build cache `<workspace>/target`;
* from the GitHub Actions jobs that build the workspace (including local
  composite actions under `.github/actions`): the container image (pinned
  `rust:1.x-<distro>` becomes `rust:1-slim-<distro>`, the toolchain file pins
  the version), `apt-get install` packages (runner-only packages such as
  `sudo` or `e2fsprogs` are listed as not adopted), job `services:` and
  PostgreSQL packages as `[services]` (`postgres:N`, `max_connections` when
  CI mentions it), download-and-install blocks (`curl -o FILE`, checksum,
  `tar`, `<tool> --version`) as setup steps, `taiki-e/install-action`
  tools with known recipes (cargo-nextest), literal workflow/job
  environment variables (CI cache paths are skipped), and the variables of
  test steps as the `[tool.cargo] env` allowlist (`APP_TEST_*`);
* without such a workflow, an obvious `FROM rust:` Dockerfile;
* git dependencies on github.com in any `Cargo.toml`: `GH_TOKEN =
  "cmd:gh auth token"` in `[secrets]`, a git credential helper setup step and
  `CARGO_NET_GIT_FETCH_WITH_CLI`;
* `[resources] cpu` from the CI runner size (`8vcpu` → 5), local cargo
  subcommands, sccache and mold.

For ~/app the result matches the hand-written configuration in
every detectable item (tested with a fixture of its files).

`cloudsourced doctor [--json]` checks and explains: `cloudsourced` in PATH
and the shims first in PATH for their tools, the client configuration, the
control plane (reachable, certificate fingerprint, API key accepted, clock
skew from its `Date` header) or the embedded pool configuration, the daemon,
the machines of the pool, the repository configuration, every secret (it is
resolved, its value is never printed), the environment image (where it
exists, whether it will be built, the last background build) and FUSE
(`/dev/fuse`, `fusermount3`; required only with `[outputs] mode = "fuse"`).
The exit status is 1 when a check failed.

### Environment image prebuild

When the environment of a repository changes (`.cloudsourced.toml`, the
toolchain file, setup `inputs`, the Dockerfile directory: anything that
changes `cloudsourced env hash`), the daemon builds the new image in the
background right away: on the file watcher event (debounced 2 s) and, when no
event was seen, at the next request. The build goes to every provider and
platform of the pool, including pushes to remote registries (ECR, OCIR) that
would otherwise wait for the first machine; with a shared control plane the
daemon uploads the context and the control plane builds detached from the
request. A run that needs the image meanwhile joins the running build.
`cloudsourced env build [--wait]` starts it explicitly, `cloudsourced env
status [--json]` shows where the image exists and the last background build;
failures are in the daemon log and `env-build.log`.

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
such as the cache TTLs travel in each request, so
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
policy            = "default"  # cost model; "binpack" / "classic" are kept for comparison
cost_optimization = 0.5        # 1 = cheapest, 0 = fastest result (see below)
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

Sizes may declare their disk (`disk = "200G"` in `[[providers.NAME.size]]`);
the scheduler then plans build slots against it.

#### Cost model and `cost_optimization`

The default policy scores every candidate of a decision (run now on a
machine, wait for a busy or starting machine, start or create a machine,
prune or grow a full disk, restore a slot snapshot) by money and time to
result and minimises

    r * cost / min_cost + (1 - r) * time / min_time      (r = cost_optimization)

normalised against the cheapest and the fastest candidate of the same
decision. `cost` is what the pool pays: the machine time a placement
occupies (full price where it extends a machine's uptime, its share where
the machine is paid anyway), started and created machines, the egress of a
snapshot restore and extra disk. `time` is queue wait + start or boot
latency + slot preparation (prune, resize, restore) + the run, using what
the estimator learned per key: warm and cold durations (cold per provider,
because global result caches live in a provider's store), restore times and
slot growth (`task_end.usage.slot_bytes_before/after`). `cost_optimization =
1` optimises purely for cost (e.g. waits for a warm slot instead of building
cold on an idle machine), `0` purely for time, `0.5` (default) accepts 10 %
more cost for more than 10 % less time. Rendezvous hashing breaks ties.
Waits are bounded by `queue_timeout`.

Other inputs of the model: provider boot/start latencies (learned), snapshot
locations (reported by workers when they save slots), per-repository slot
sizes and the machine disk, and transfer prices between stores and providers:

```toml
[[scheduler.egress]]
from         = "aws"      # store: provider name, or "storage" for [storage]
to           = "oci"      # provider whose machines restore
price_per_gb = 0.09
bandwidth    = "50M"      # bytes/s
disk_price_per_gb_month = 0.08   # [scheduler]: prices disk resizes
```

Unlisted pairs are free within a provider's own store and cost 0.09/GB at
50 MB/s across. A full slot is handled by the cheaper/faster of: prune other
slots (their next builds become cold, which the model charges), grow the disk
(`provider.DiskResizer`, aws-ec2), or run elsewhere (cold, or restore a
snapshot). Capacity and quota errors take a provider out of scale-out for a
cooldown (2 min, doubling up to 30 min, reset on the next ready machine); a
failed start backs off for 2 minutes instead of retrying in a loop.

Consolidation is coordinated with creation: a machine is only drained when
the peak demand of the last 5 minutes (running plus queued) fits on the
others, never within 2 minutes of a scale-up, a drained machine is undrained
before anything is started or created, and an emptied consolidated machine
whose demand the others cannot take is undrained instead of stopped. This
fixes the drain-then-create flapping seen in production
(`internal/scheduler/sim/testdata/prod-flap.json`, regression test
`TestRegressionDrainThenCreate`).

`cloudsourced sim -sweep trace.json` sweeps `cost_optimization` from 0 to 1
and prints the cost/time frontier (Pareto points marked); `sim
-cost-optimization 0,0.5,1` compares chosen values, `-resize` lets
machines grow disks. Traces exported with `sim export` carry slot sizes,
so disk, slot growth, restores and multiple providers are simulated.

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

[cache.target]
scope    = "slot"           # worker-local build slot, never synchronised
paths    = ["target"]
ttl      = "24h"            # per build slot, capped by the pool's max_lifetime
max_size = "100G"           # slot budget, enforced by pruning
prune    = "rust"           # rust | lru (default) | none

[cache.deps]
scope = "machine"           # shared by the sessions of a machine
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]
ttl   = "7d"                # lifetime of the machine cache a new machine starts

[cache.node]
scope   = "machine"
paths   = ["web/node_modules"]     # generated remotely, hidden from the projection
digest  = ["web/bun.lock"]         # immutable per content hash (+ platform)
install = "cd web && bun install --frozen-lockfile"

[cache.sccache]             # shared compiler cache across machines
scope = "global"
tool  = "sccache"           # sccache | ccache | go; installs the tool if needed
ttl   = "12h"               # optional, at most 24h

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
# split = ["test", "nextest"]               # default: remote build, local test run
# adapter = "cargo"                         # default from the tool name

[tool.npm]
ports = [8080, "3000:3001"]  # forward 127.0.0.1:local -> session port while it runs
```

Then just use the tool:

```sh
cargo build
cargo test
cloudsourced status                 # machines, sessions (with labels), cost today (--json)
cloudsourced top                    # live view: graphs, machines, sessions, queue, cost
cloudsourced why                    # last run: local/remote and why, machine,
                                    # placement, timings, usage, labels (N, list, --json)
cloudsourced prewarm                # start the remote environment of this repo now
cloudsourced hooks install          # prewarm automatically on new worktrees
cloudsourced fetch 'target/debug/deps/*.so'   # lazy outputs
cloudsourced outputs mount          # remote target/ at its local path (writable overlay)
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

### Static hosts

`type = "static"` uses machines you already run (bare metal, a homelab,
reserved instances) with sshd, Docker and systemd:

```toml
[providers.lab]
type        = "static"
registry    = "registry.lab:5000/cloudsourced-env"  # hosts pull environment images from here
ssh_key     = "file:~/.ssh/cloudsourced"            # default file:~/.ssh/id_ed25519
known_hosts = "~/.ssh/known_hosts"                  # host keys are verified
hosts = [
  { name = "build1", ssh = "root@10.0.0.5", cpu = 32, memory = "128G" },
  { name = "build2", ssh = "root@10.0.0.6:2222", addr = "10.0.0.6:7443", cpu = 16, memory = "64G", arch = "arm64" },
]

[[providers.lab.size]]
name = "lab"
cpu = 16
memory = "64G"
```

The orchestrator places one machine per host (the first free host with
enough CPU and memory) and drives it over SSH: it uploads its own binary to
`/var/lib/cloudsourced/bin/` once, then runs `<binary> supervisor static
apply|start|stop|destroy|status`. `apply` writes the public machine spec,
the secret environment (root-only, read once by the supervisor, which is
the static provider's secret channel) and the `cloudsourced-supervisor`
unit; the supervisor starts the containers as on a VM. At the end of a
machine's lifetime the supervisor removes the machine instead of powering
the host off; `destroy` does the same on request. Hosts must be logged in
to `registry` (images are built locally and pushed there) and reachable
on UDP 7443. Static hosts have no native object store: configure
`[storage]` for snapshots and the shared result cache.

### Output overlay (output FUSE)

By default only `eager` outputs come back after a run; `cloudsourced fetch`
copies others on request. With the output overlay the remote build
directories (`[cache.build] paths`, e.g. `target/`) appear at their local
path as if they had been built locally: local `cargo run`, `cargo test`,
`cargo nextest` use the remote artifacts without rebuilding and without
`cloudsourced fetch`.

```toml
[outputs]
mode = "fuse"      # mounted by the daemon for this repository
```

```sh
cloudsourced outputs mount      # mount by hand (also without mode = "fuse")
cloudsourced outputs status     # mount points, upper layers, generation
cloudsourced outputs unmount    # unmount and discard local writes
```

- The mount (owned by the daemon, `fusermount3`, file system type
  `fuse.cloudsourced-outputs`) is a writable union implemented with go-fuse
  (`internal/outputfs`). Lower layer: the build slot of the latest remote
  build of the repository. Upper layer: `$STATE/outputs/upper/<hash>`, a
  local directory next to the client state, never in the repository; every
  write goes there (cargo's locks and fingerprints, local rebuilds, copied
  up files). The local `target/` below the mount point is hidden while
  mounted and untouched; it is all there again after `unmount`.
- Kernel overlayfs is not used: an unprivileged user can only mount it in a
  user namespace, invisible to the shell that runs cargo.
- Directories are listed on first access from the machine of the latest
  build, file contents are downloaded whole on first open into a local content
  cache (`$STATE/outputs/cas`, keyed by BLAKE3) and reused as long as path,
  size and mtime are unchanged. Every remote run starts a new generation:
  listings and kernel caches are dropped, unchanged contents are kept.
  Exact mtimes (nanoseconds) are served, which cargo compares.
- Layer rules: for a path present in both layers the newer mtime wins, ties
  go to upper (latest writer wins). Writing a remote file copies it up with
  its remote mtime (`O_TRUNC` without download); deleting or replacing a
  remote entry writes a whiteout marker `.wh.NAME` whose mtime hides remote
  entries up to that time; a directory created over a deleted remote one
  gets an opaque marker. So a newer remote build replaces older local state
  at the paths it produced, and local rebuilds win over older remote
  outputs. Names starting with `.wh.` cannot be created in the mount.
  Directories with remote content cannot be renamed (`EXDEV`, as overlayfs
  without `redirect_dir`); `mv` falls back to copying. cargo's uplift of a
  fresh output (unlink `debug/x`, hard link `deps/x-HASH`) does not download
  anything when the remote build has the same link.
- Eager outputs inside a mounted directory are stored in the content cache
  instead of being written below the mount point, so they are served
  without the machine.
- When the machine is stopped or gone, cached listings and contents are
  still served; uncached files fail with `EIO`, uncached directories look
  empty, and the daemon log says which machine was unreachable. The machine
  is not asked again for 20 s.
- The mount and its upper layer survive daemon restarts (state in
  `$STATE/outputs/state.json`).
- Go API: `daemon.EnsureOutputs(ctx, root)` mounts the overlay of a
  repository (starting the daemon) and returns its status; it is current as
  soon as a remote run returned.

**Cargo freshness.** Local cargo considers the remote build fresh when its
fingerprints match. With `mode = "fuse"` every remote session of the
repository aligns what cargo hashes:

- Sources are projected at the local path (identical absolute paths, mtimes).
- The image's `CARGO_HOME` and `RUSTUP_HOME` appear at the client's paths
  (`$CARGO_HOME`/`$RUSTUP_HOME` or `~/.cargo`/`~/.rustup`), bind mounted in
  the session's mount namespace; `PATH` and the variables point there.
  Cargo hashes the absolute paths of registry sources
  (`$CARGO_HOME/registry/src/...`), so `$CARGO_HOME/registry` caches
  (`[cache.deps]`) also live at the client's path. The path of `rustc`
  itself is not hashed, its version (`rustc -vV`) is: pin it with
  `[environment.toolchains] rust` and use the same `rust-toolchain.toml`
  locally.
- The configured linker (`[environment] linker`) is selected through a `cc`
  on the session's `PATH` instead of `CARGO_TARGET_<triple>_LINKER`, which
  cargo hashes.
- `build.rustflags` and `target.<host>.rustflags` from the client's
  `$CARGO_HOME/config.toml` (not projected) are passed as
  `CARGO_BUILD_RUSTFLAGS`/`CARGO_TARGET_<HOST>_RUSTFLAGS` unless the run
  forwards `RUSTFLAGS` or those variables. They must work remotely (e.g. a
  `-fuse-ld=mold` needs `linker = "mold"`).

Measured with `test/bench/overlay-fresh.sh` (local-docker, a crate with a
proc macro, `itoa` and `libc` with a build script, a test; local cargo with a
global config setting `rustc-wrapper` and target rustflags): after a remote
`cargo build` and `cargo test --no-run`, local `cargo build` and
`cargo test --no-run` recompile 0 of 5 units, the test and binary run
locally, and after an edit and a remote rebuild local cargo again
recompiles 0 units; a local edit recompiles only the edited crate (into
the upper layer).

Known gaps (cargo rebuilds the affected units locally, which is correct but
slower): environment variables that differ locally and are tracked by
`env!`/`option_env!` or a build script's `rerun-if-env-changed` (e.g. `CC`
from a local `[env]` section); `CARGO_INCREMENTAL=0` set only remotely
(set it locally too); other settings of the local
`$CARGO_HOME/config.toml` that cargo fingerprints (`target.<triple>.linker`,
`[profile]` overrides); a different local toolchain version.

### Caches

Every `[cache.<name>]` table of the repository configuration is one cache
with a `scope`:

| scope | lives | keys |
| --- | --- | --- |
| `slot` | in the session's exclusive build slot on the machine, reused warm by the next session of the repository | `paths` (plain directories inside the repository), `ttl` (default 24h), `max_size`, `prune` |
| `machine` | in the machine cache directory, shared by all sessions of the machine | `paths` (`$VAR`, `~`, absolute or inside the repository), `ttl` (default 7d), `max_size`, `prune`; with `digest` + `install`: generated per lockfile |
| `global` | in the pool's store, shared by all machines | `tool` (`sccache`, `ccache`, `go`), `ttl` (at most 24h) |

Without `scope`, a table with `tool` is global, otherwise slot. Slot and
machine paths are hidden from the source projection (they never come from
the laptop).

Global caches use the result-cache endpoint of the machine (see
[Compiler cache and linker](#compiler-cache-and-linker)): `sccache` sets
`RUSTC_WRAPPER`, `ccache` installs the Debian package and points
`CCACHE_REMOTE_STORAGE` at the endpoint (local `CCACHE_DIR` on the machine
disk), `go` sets `GOCACHEPROG` to `cloudsourced gocacheprog`, which keeps
outputs on the machine disk (go reads them from there) and shares them through
the endpoint. A `ttl` below 24h lowers the age of reused entries.

#### Machine caches per digest (`node_modules`)

A machine cache with `digest` and `install` is immutable per content hash:
the digest covers the matching files (globs, e.g. the lockfile), the
cache's name, paths and install command, and the environment's platform
(`os/arch/libc`). The first session that needs a digest runs `install`
remotely (sandboxed, with the session's environment and the cache paths
bound to a fresh directory; its output appears on stderr) and stores the
result in the machine store `<deps dir>/digest/<name>/<digest>`. Every slot
gets a copy linked from the store on the same disk: reflinks where the
filesystem supports them (writable), otherwise hard links mounted
read-only, so sessions can never change the store. The copy is refreshed
when the digest changes. The paths are hidden from the source projection,
so a local `node_modules` (e.g. for another platform) never reaches the
machine. Digests unused for the cache's `ttl` are removed, and per cache
the least recently used ones beyond `max_size` (the newest stays). Plain
machine caches with `max_size` are pruned by least recent access while the
machine is idle.

#### Pruning

Slots are pruned against two budgets: per slot the sum of the slot caches'
`max_size` (unset: unbounded) after every session, per machine
`CLOUDSOURCED_DISK_BUDGET` bytes of the cache filesystem (default 90%).
The worker tracks reads per session: after a run it records for every slot
file the last run (session number of the slot) that read or wrote it, using
the atime (relatime re-armed by resetting the atime of read files to their
mtime; on `noatime` mounts only writes count). Pruning runs in stages until
the slot fits:

1. `prune = "rust"`: old incremental compilation sessions (all but the
   newest per crate in `target/*/incremental/`);
2. units not read for 5 runs, the stalest first (`rust`: all files of one
   compilation unit, i.e. `deps/*-<hash>.*`, `.fingerprint/<unit>/`,
   `build/<unit>/`; `lru`: single files); with a size target also more
   recently read ones, never those of the latest run;
3. the whole slot (cold; the next session starts a new one).

Over the machine budget the least recently used slots get the policy prune
(stages 1 and 2) first, then go cold. The orchestrator can prune a
repository's slots on demand through the worker's slot stream
(`proto.KindSlot`, admin token; `orchestrator.WorkerSlotOps.PruneSlot`):
older duplicate slots go cold first, then the newest is pruned in stages to
the requested size.

### Object store (`[storage]`) and provider parts

Internally a provider has four parts: **Compute** (machines), **Registry**
(environment images), **Store** (an S3-compatible object store for slot
snapshots and global caches) and **Secrets** (the channel that hands a new
machine its secret environment). The pool's `[storage]` section replaces
the Store part of every provider:

```toml
[storage]
endpoint   = "https://<account>.r2.cloudflarestorage.com"   # "" = AWS S3
bucket     = "cloudsourced-cache"
region     = "auto"
prefix     = "cache/team"           # default "cache/<pool id>"
access_key = "env:R2_ACCESS_KEY_ID" # env:/file:/cmd: references
secret_key = "env:R2_SECRET_ACCESS_KEY"
# role_arn   = "arn:aws:iam::123:role/cache"  # STS: per-repository credentials
# path_style = true                            # MinIO
# dir        = "/srv/cloudsourced-store"       # a directory every machine mounts
```

Native stores: `aws-ec2` uses the stack's bucket below `cache/<pool>/`
(one-day lifecycle) and the stack's `StoreRole`; `local-docker` a volume
`cs-store-<pool>` shared by its containers (`/store`); `oci` has none (use
`[storage]`; OCI customer secret keys cannot be scoped per repository).

The control plane never moves cache data. With every lease (and every slot
save or restore) it hands the worker a **grant**: credentials scoped to the
repository's prefix (`repos/<repo id>/`) and the environment's
(`envs/<env hash>/`), at most 12 h. With `role_arn` (and on `aws-ec2`) they
come from STS AssumeRole with a session policy limited to those prefixes;
otherwise the configured keys are handed out as they are, so give them only
the bucket (or prefix) they need. Objects are build caches and never served
after 24 h.

Global result caches (sccache, ccache, Go) use a tiered store in the
worker: a directory on the machine disk, backed by the grant's environment
prefix, so a fresh machine reuses what earlier machines compiled. Without a
store they stay machine-local.

### Snapshots and lazy restore

A build slot (and a machine cache per digest) can move between machines
through the store. `internal/snapshot` saves a directory as content
addressed chunks (BLAKE3; small files packed into ~8 MB chunks with
content-defined boundaries, large files split) plus a manifest with every
file's mode, size, mtime and chunk ranges (`repos/<repo>/snapshots/<unix>.json.gz`,
newest named by `snapshots/latest`). A save reuses the chunk ranges of the
slot's previous save for files whose size and mtime did not change, so it
reads and uploads only what changed; chunks older than 12 h are uploaded
again, so a snapshot stays restorable although the store expires objects
after 24 h.

A restore creates a new slot at once: directories, symlinks and sparse
placeholders with the final sizes and the original mtimes (cargo compares
mtimes, so a restored `target/` is fresh). File contents are filled on
first access through a FUSE fill layer mounted on the slot's data
directory (a loopback over the local directory that fetches a file before
it is opened, truncated or renamed) and by a background fill; once
complete and not in use, the mount is replaced by the filled directory. The
live cache is always on local disk. Without FUSE the restore fills
everything before it returns. The slot keeps its original creation time,
so the 24 h bound holds across machines.

The scheduler decides when (`orchestrator.SlotOps`): `SaveSlot` and
`RestoreSlot` call the worker's slot stream with a fresh grant; the first
session on a restored slot reports `usage.restored` and
`usage.restore_seconds`. A worker draining at the end of its lifetime saves
the slots of every repository it holds a grant for and lists them in
`Status.Snapshots`. Machine caches with a digest are saved to
`envs/<env>/digests/<name>/<digest>/` after their install, and other
machines restore them instead of running the install.

Measured with `test/bench/snapshot.sh` (this host, local directory store,
small crate with serde, serde_json, itoa, libc):

| step | result |
| --- | --- |
| `target/` | 125.7 MB, 189 files, 24 chunks |
| cold build (sccache filling) | 8.36 s |
| snapshot save, first / incremental (one crate changed) | 0.18 s / 0.02 s (24 / 1 chunks uploaded) |
| restore: skeleton / first file / full fill | 0.00 s / 0.00 s / 0.09 s |
| units rebuilt by `cargo build` after the restore | 0 |
| rebuild from scratch with the warm sccache | 3.29 s |

With a remote store the fill is bounded by download bandwidth (about 1 s per
125 MB at 1 Gbit/s); point `BENCH_STORE` at a mounted bucket to measure it.

#### Disks: growing, gp3 versus local NVMe

`aws-ec2` implements `provider.DiskResizer`: the scheduler can grow a full
machine's gp3 root volume (`ec2:ModifyVolume`, once per six hours, never
shrinking) and the supervisor grows partition and file system
(`growpart`, `xfs_growfs`/`resize2fs`) on its next check.

Machines use gp3 root volumes (`disk_gb`, default 100; 3000 IOPS and
125 MB/s baseline). Instance types with local NVMe (`c6id`, `m6id`, `c7gd`)
are faster for target directories but lose their content on stop, so their
warm state only survives through snapshots. To compare on a real machine
(no cloud run is part of the tests):

1. Start one machine per disk type (e.g. sizes on `c6i.2xlarge` and
   `c6id.2xlarge`); for NVMe mount the instance store at the cache dir.
2. Raw disk: `fio --name=rand --rw=randrw --bs=16k --size=4G --numjobs=8
   --iodepth=32 --direct=1 --runtime=60 --group_reporting --filename=/cache/fio`
   and `--rw=write --bs=1M` for sequential throughput.
3. Builds: run the same `cargo build` cold, with warm sccache and warm slot
   on each, and compare `task_end.usage` (`wall`, `cpu_stall_seconds`)
   with `cloudsourced report --by size`.
4. Restore: `test/bench/snapshot.sh` with `BENCH_STORE` on the machine
   measures save and fill against each disk; gp3 throughput (125 MB/s
   unless provisioned higher) bounds the fill of large slots.

### Compiler cache and linker

A global cache with `tool = "sccache"` adds the pinned static sccache release
(checksum verified, downloaded once on the client into the user cache
directory, `CLOUDSOURCED_TOOL_CACHE` overrides) to the environment image.
Each session gets its own sccache server on a unix socket inside the session
sandbox (`SCCACHE_SERVER_UDS`, `SCCACHE_DIR` in a private temporary
directory); it ends with the session. The cache backend is WebDAV
(`SCCACHE_WEBDAV_ENDPOINT`), served by `internal/blobcache`:

The worker serves it on `127.0.0.1` from a tiered store: a directory under
`<deps dir>/sccache` (`CLOUDSOURCED_SCCACHE_DIR`) backed by the pool's
object store under the environment's prefix (see
[Object store](#object-store-storage-and-provider-parts)), so a new machine
gets hits for everything an earlier one compiled. Keys are prefixed with
the environment hash, so toolchains never mix.

sccache cannot cache incremental compilations, which cargo uses for
workspace crates in dev profiles; dependencies are cached regardless.
Set `CARGO_INCREMENTAL=0` in `[environment.env]` to make them cacheable.
User settings of `RUSTC_WRAPPER` win. With `CLOUDSOURCED_VERBOSE=1` the proxy
prints the run's statistics (`sccache: 3 hits / 1 misses (...)`); they are
also part of the session's exit message (`proto.Exit.Cache`: `sccache_requests`, `_hits`, `_misses`, `_not_cacheable`, `_errors`; shown by `cloudsourced why`).

`[environment] linker = "mold"` installs the pinned static mold release
(`lld` installs the distribution's `lld` package, Debian based images) and a
driver `/usr/local/bin/cloudsourced-cc-<linker>` (`cc ... -fuse-ld=<linker>`, last so it beats
rustc's own `-fuse-ld=lld` for its bundled rust-lld),
which sessions use as `CARGO_TARGET_<host triple>_LINKER` unless set (with
`[outputs] mode = "fuse"` through a `cc` on `PATH` instead, see above). This
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
  output overlay, which the command mounts. The paths rust-analyzer gets from
  the remote `cargo check --message-format=json` (proc-macro `.so`, `OUT_DIR`)
  resolve inside the mount. `cargo metadata` stays local (dependency sources
  must exist locally). The local toolchain must match the remote one
  (`rust-toolchain.toml`), otherwise rust-analyzer's proc-macro server
  rejects the libraries.

### Tool adapters

Every proxied command goes through the adapter of its tool
(`internal/adapter`): it classifies the command line into phases — `deps`,
`build`, `run` — each `local` or `remote`, names the artifacts a phase
produces with their cache scope (slot, machine, global, or transferred
here for a local phase) and what a local phase needs. `[tool.X]` stays the
per-tool configuration (`local`, `remote`, `env`, `eager`, `ports`,
`split`); `adapter = "cargo" | "go" | "node" | "generic"` picks the
adapter, by default from the tool name (cargo; go; bun, bunx, npm, npx,
pnpm, yarn; anything else generic). `cloudsourced why` shows the adapter
and the phases of a run (`adapter`, `phases` in `runs.jsonl`).

| adapter | remote as typed | split: remote build, local run | default caches (`init`) |
|---|---|---|---|
| cargo | build, check, clippy, run, ... | `cargo test`, `cargo nextest run` | `target` slot (prune rust), `deps` machine, `sccache` global |
| go | build, vet, run, mod, ... | `go test` | `go-build` global (GOCACHEPROG), `go-mod` machine, `go-out` slot |
| node | install/ci/add (deps), scripts, tests | — (`local = ["test", "run dev"]` runs them here) | `node-modules` machine, digest of the lockfile, install command |
| generic | everything not in `local` | — | — |

Local platform checks (OS, architecture, C library family and version,
dynamic loader, shared libraries) use `internal/platform`, which also feeds
the environment image hash (`platform=linux/<arch>`) and the cache key of
machine caches (`Platform.CacheKey()`: OS/arch/libc without version).
`CLOUDSOURCED_PLATFORM=linux/amd64/glibc-2.39` overrides the detection.

### Tests: remote build, local run

`cargo test`, `cargo nextest run` and `go test` are split: the test
binaries are built remotely and run on this machine, with the local
environment and local services (e.g. the developer's Postgres via
`APP_TEST_DATABASE_URL`). Type the command as usual (`cargo nextest run
-p app-store foo`, `cargo test foo -- --nocapture`, `go test -run X
./...`); output and exit code are those of a local run.

* **cargo test**: the remote phase runs `cargo test --no-run
  --message-format=json-render-diagnostics` with the build options; the
  executables it reports are transferred and run in cargo's order with
  cargo's working directory (package root), environment (`CARGO_MANIFEST_DIR`,
  `CARGO_PKG_*`, `CARGO_BIN_EXE_*`, cargo `[env]` configuration including
  `force` and `relative`, the build script's `rustc-env` values and
  `OUT_DIR`, whose files are transferred too), arguments (test name,
  `--quiet` with `-q`, everything after `--`), status lines and `error: test
  failed, to rerun pass ...` messages, `--no-fail-fast` summary and exit
  code 101. Doc tests are compiled while they run, so they run remotely
  afterwards (`cargo test --doc` with the same options), as cargo runs them
  last.
* **nextest**: the remote phase runs `cargo nextest archive` with the cargo
  build options (`-p`, `--workspace`, features, `--cargo-profile`, ...); the
  zstd archive is transferred and `cargo nextest run --archive-file ...`
  runs locally with the filters, runner and reporter options, `-P`/
  `--profile`, `-- args` and `NEXTEST_*` variables. Configuration options
  (`--config-file`, `-P`) go to both.
* **go test**: the remote phase lists the packages and runs `go test -c`
  per package with the build flags (`-race`, `-tags`, ...; results come
  from the global go cache); the binaries run locally in their package
  directories with the test flags as `-test.*` (`-run`, `-v`, `-count`,
  `-timeout`, ..., `-args`), printing go test's `ok`/`FAIL`/`?` lines and
  exiting 1 on failure. The binaries travel in the first slot-scope cache
  directory (`init` proposes `[cache.go-out] paths = [".cloudsourced-build"]`).

Commands that do not run tests (`--no-run`, `--doc`, `--message-format`,
`nextest list`, `nextest archive`, reuse options such as `--archive-file`;
`go test -json`, `-c`, `-o`, `-cover*`, profiles, `-fuzz`, `-list`) and
unknown options run remotely as typed. The tests run remotely instead,
with one line saying why, when this machine cannot execute the binaries:
another OS or architecture, another C library or an older glibc than the
environment image, a missing shared library or dynamic loader (checked on
the ELF files), no local `cargo-nextest`, no `cargo-nextest` in the
environment image, nextest failing to list the tests locally (exit 104),
or test binaries outside the slot-scope cache paths.

`split` lists what is split (cargo: `["test", "nextest"]`, go: `["test"]`,
both the default); `split = []` turns it off for a tool,
`CLOUDSOURCED_SPLIT=0` for one run. Transport: with `[outputs] mode =
"fuse"` the binaries appear through the output overlay
(`daemon.EnsureOutputs` after the build's release, nothing is copied);
otherwise they are fetched over the session that built them, zstd
compressed, only files whose content changed (`cloudsourced fetch` uses
zstd too). Not reproduced locally: files a test reads at run time from
outside `OUT_DIR` and the build directories (e.g. generated next to the
sources by a remote build script), cargo `[env]` set by `--config` on the
command line, and nextest's own handling of build-script values (whatever
`cargo nextest run --archive-file` does locally).

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

**Machine loss.** If a session ends without an exit status because the
connection to its machine broke (the machine stopped, was destroyed or became
unreachable) and the machine does not answer a new connection, the proxy runs
the command once more on another machine and prints one line saying so; the
run log records `lost_machine`. Write-back stays exactly-once: local files
only change in the write-back phase of the attempt that reported an exit
status, so the lost attempt has applied nothing. There is no retry when the
write-back had begun, standard input had been forwarded (it cannot be
replayed), the user signalled the command, the session had not started yet
(that falls back as before), the worker reported an error, the machine still
answers, after a second loss, or with `CLOUDSOURCED_NO_RETRY=1`. Output the
first attempt already printed stays on the terminal.


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

### Agents, worktrees and labels

Every session carries labels: `worktree` (the repository root, i.e. the
worktree), `repo` (name of the main repository, shared by its worktrees),
`branch`, `user` (client config `user`) and `agent`. They are computed by the
proxy (git metadata is read from `.git` files, git is not run), travel with
the session to the daemon, the orchestrator and the worker, and are recorded
in the event log (`labels` of `task_placed`/`task_end`) and the run log.
`cloudsourced why`, `status`, `top` and `report` show them.

`agent` is `CLOUDSOURCED_AGENT` if set, otherwise detected from the first
present variable: `CODEX_THREAD_ID` (codex, the thread id also becomes
`agent_session`), `CODEX_SANDBOX`, `CODEX_MANAGED_BY_NPM` (codex),
`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT` (claude-code), `CURSOR_AGENT`
(cursor), `GEMINI_CLI` (gemini-cli), `OPENCODE` (opencode), `AIDER_MODEL`
(aider), `AMP_THREAD_ID` (amp, also `agent_session`). Only these names and
ids are read, never other environment values. `CLOUDSOURCED_LABELS="task=WP-7,team=infra"`
adds free labels. Keys are `[a-z0-9_.-]{1,32}`, values at most 200
characters, at most 16 labels; servers clean labels again on receipt.
Labels never influence placement.

`cloudsourced report` groups cost (attributed by reserved share, as per
repository), session-hours, CPU-hours, requests and failures, queue wait
p50/p95/max and contention (share of wall time stalled on CPU/memory) by
agent and by worktree; `--by agent|worktree|branch|repo|user` (comma
separated) chooses the dimensions. Events from before labels fall back to
the repository id; sessions without an agent count as `(human)`.

**Prewarm.** `cloudsourced prewarm` starts the remote environment of the
current repository in the background (log `<state>/prewarm.log`): image
ensure, machine lease and start, source sync and the build slot, then an
optional warm-up command from the repository configuration:

```toml
[prewarm]
run = ["cargo", "check", "--workspace", "--tests"]
```

`--wait` runs in the foreground, `--force` skips the warm check. A prewarm
is skipped when a running machine of the environment already has a warm
build slot of the repository and (with `run`) the warm-up succeeded for the
same commit, environment and command within 12 hours. Only one prewarm per
repository runs at a time. Prewarm sessions are labelled `purpose=prewarm`
(shown in `why` and `report`), never fall back to local execution and are
not counted as user runs in the daemon metrics. `cloudsourced hooks install`
installs a git `post-checkout` hook into the repository's hooks directory
(shared by all worktrees, honours `core.hooksPath`) that calls
`cloudsourced prewarm --quiet` after branch checkouts, which includes `git
worktree add`. An existing hook is chained (renamed to
`post-checkout.cloudsourced-chain`, runs first, its exit status is kept);
install is idempotent and `cloudsourced hooks uninstall` restores the
previous hook.

**Top.** `cloudsourced top` is a full-screen view in the style of zenith:
history graphs of the pool (busy cores, session memory, sessions plus queue,
cost per hour; seeded from the `machine_sample` events and extended by the
2 s status polls, kept in memory for 3 h), one line per machine (busy-core
bar and history, memory bar, PSI, sessions, price), a sortable session table
(machine, agent, worktree, branch, elapsed, busy/reserved cores,
used/reserved memory, command) and the queue. Keys: ↑/↓ PgUp/PgDn select,
`e c m a w n` sort by elapsed, CPU, memory, agent, worktree, machine (again:
reverse), `r` reverse, `+`/`-` zoom the graph window (1m..3h), `q` quit.
`cloudsourced top --once [--width N] [--window 10m]` prints one plain text
frame without colors, for agents and scripts. Busy cores per session come
from the session's cgroup CPU time between polls (`Status.Sessions[]
cpu_seconds`, `used_memory`).

**Event log and reports.** Independent of tracing, the orchestrator writes
an analysis event log (`events-YYYY-MM-DD.jsonl` in its state directory,
`[telemetry] events_retention_days`, default 30) of task and machine events.
`cloudsourced report [--since 7d] [--by agent,worktree] [--json]` shows cost per day, repository
and size, per agent/worktree/branch/repo/user, utilisation and overprovisioning, queue wait p50/p95, warm vs.
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
| machine caches | `ttl` of `scope = "machine"` caches (default 7 days) | volume expiry label, orchestrator reaps volumes |
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
