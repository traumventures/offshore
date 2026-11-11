# Configuration

offshore reads three kinds of TOML files. Every file is decoded
strictly: unknown keys are errors, there are no aliases or legacy keys.

| File | Role | Read by |
|---|---|---|
| client: `~/.config/offshore/config.toml` (`$OFFSHORE_CONFIG`) | where the orchestrator runs (`[control_plane]`, absent = embedded), `user`, `[telemetry]` of proxy and daemon, `[outputs]` output cache | CLI, daemon |
| pool: `$OFFSHORE_POOL_CONFIG`, `controlplane --config FILE`; default: the client file | `[pool]`, `[scheduler]`, `[[machine.setup]]`, `[providers.*]`, `[storage]`, `[telemetry]` | the orchestrator: daemon in embedded mode, control plane |
| repository: `.offshore.toml` at the repository root (plus an optional, not checked in `.offshore.local.toml` that overrides it) | environment, resources, caches, tools, services, secrets | proxy and daemon; sent with every request |

In embedded mode one file may hold the client and the pool sections. With a
`[control_plane]` the pool sections of the client file have no effect (the
client warns); the control plane ignores `[control_plane]` and `user` in its
pool file.

Pool settings change only by reload: the daemon applies a changed pool file
on the next request and on `offshore reconcile`; the control plane when
the file changes (checked before each lease and every 15 s), on `SIGHUP` and
on `reconcile`. `pool.id`, the providers and `scheduler.estimator` apply
only after a restart. Repository settings travel with each request, so
repositories with different settings share one pool.

```sh
offshore config check               # validate the files of this mode, name their roles
offshore config show                # effective configuration, secrets redacted
offshore config check --pool FILE   # only a pool file, as the control plane reads it
```

Values: sizes are `512M`, `16G`, `1T` or bytes; durations are Go durations
plus `d` for days (`20m`, `24h`, `7d`); secret references are `env:VAR`,
`file:PATH` or `cmd:COMMAND`.

## Client configuration

```toml
user = "alice"                           # salts repository ids (default $USER)

[control_plane]                          # absent: the daemon embeds the orchestrator
url         = "https://10.0.0.5:7443"    # or https://cs.example.com
fingerprint = "0b91…"                    # only for a self-signed certificate
api_key     = "env:OFFSHORE_API_KEY" # reference: env:, file:, cmd:
client_cert = "client.pem"               # only with controlplane --client-ca
client_key  = "client-key.pem"           # relative to this file

[telemetry]                              # tracing of proxy and daemon, see operations.md
endpoint = "https://otel.example.com:4318"

[outputs]
cache_size = "8G"                        # the daemon's cache of remote outputs
```

| client key | default | |
|---|---|---|
| `outputs.cache_size` | `"8G"` | bound of the daemon's output cache (anyfs pages and materialized files of remote outputs, each reclaimed least recently used beyond it, [tools.md](tools.md#output-overlay)). Outputs the laptop fetched that the machine has not committed yet are kept regardless, until the store holds them or a newer build replaced them |

The repository's `[outputs] mode` selects how outputs appear; the client's
`[outputs]` only sizes the cache. The pool loader ignores the client
`[outputs]` (with a warning) when one file holds both.

`offshore controlplane client-config` prints the `[control_plane]`
block, see [control-plane.md](control-plane.md).

## Pool configuration

```toml
[pool]
id             = "alice"   # labels machines and volumes (offshore.pool); default $USER
machine_cpu    = 8         # size of the implicit "default" size
machine_memory = "16G"
min_machines   = 0         # warm machines per recently used environment
max_machines   = 4         # whole pool
idle_stop      = "20m"     # stop idle machines (disk kept)
max_lifetime   = "24h"     # destroy machines (and their caches) after this
queue_timeout  = "2m"      # a request waiting longer for capacity fails

[scheduler]                # see scheduling.md
policy            = "default"   # option policy (costmodel, binpack, classic for comparison)
cost_optimization = 0.5         # 1 = cheapest, 0 = fastest result
estimator         = "history"

[[machine.setup]]          # runs once per machine before it accepts sessions
run = "echo ready"

[providers.local]
type    = "local-docker"
command = ["sudo", "-n", "docker"]

[[providers.local.size]]
name           = "small"
cpu            = 4
memory         = "8G"
price_per_hour = 0.1
```

| `[pool]` key | default | |
|---|---|---|
| `id` | `$USER` (sanitised) | 1-63 letters, digits, `.`, `_`, `-`; an orchestrator only manages resources of its own pool |
| `machine_cpu`, `machine_memory` | `8`, `"16G"` | capacity of the size `default` that providers without sizes get |
| `min_machines` | 0 | warm pool per environment used in the last 24 hours |
| `max_machines` | 4 | upper bound of the pool (also for starting stopped machines) |
| `idle_stop` | `"20m"` | idle machines stop; disks and warm slots survive |
| `max_lifetime` | `"24h"` | machines are destroyed at this age; caps the slot TTL |
| `queue_timeout` | `"2m"` | requests that cannot be placed within this time fail; the proxy then runs the command locally (unless `OFFSHORE_REQUIRE_REMOTE=1`) |

`[scheduler]` and `[[scheduler.warm_pool]]` are documented in
[scheduling.md](scheduling.md), `[storage]` in
[caches-and-storage.md](caches-and-storage.md#object-store-storage), and
`[telemetry]` in [operations.md](operations.md#tracing).

`[writeback]` sets when workers commit build slots and write their pages
back to the store
([caches-and-storage.md](caches-and-storage.md#build-slots-on-anyfs-commit-writeback-lazy-restore)):

| `[writeback]` key | default | |
|---|---|---|
| `dirty_expire` | `"30s"` | a slot idle this long is committed and written back |
| `dirty_ratio` | `0.1` | a session that grows a slot by more than this fraction of the disk budget is committed right after it ends |

### Providers

Every `[providers.NAME]` table is one provider instance; `type` selects the
backend, `max_machines` caps the machines of this provider (unset: only
the pool's `max_machines` applies; `0` disables the provider: no machine is
created or started there, its machines take no new sessions and are
destroyed once their running sessions end), every other key is a provider
option. Without `[providers]` the pool has one local-docker
provider `local` (command `$OFFSHORE_DOCKER`, default `docker`).

Sizes are `[[providers.NAME.size]]` tables:

| size key | default | |
|---|---|---|
| `name` | required | your name for the size (on aws-ec2 also the default instance type) |
| `cpu`, `memory` | required | capacity used for scheduling (vCPUs, bytes) |
| `price_per_hour` | 0 (unknown) | used for sizing decisions, `status` and `report` |
| `disk` | unknown | machine disk (`"200G"`); the scheduler plans build slots against it |
| `disk_budget` | `[scheduler] disk_budget` | cache disk machines of this size fill before pruning (`"85%"`, `"150G"`) |
| `arch` | `amd64` (local-docker: the host) | `amd64` or `arm64`; repositories pick it with `resources.arch` |
| `instance_types` | aws-ec2: the size name; oci: `VM.Standard.E5.Flex` / `VM.Standard.A1.Flex` | provider machine types tried in order on capacity errors (aws-ec2, oci only) |
| `medium` | `local` | where the cache lives: `local` (root disk, or the instance-store NVMe disk of instance types that have one, formatted on every boot; aws-ec2 takes `disk` from the instance type then) or `volume` (an attachable volume, `[providers.NAME.volume]`; aws-ec2, oci, local-docker) |
| `stack` | from `medium`: `["local", "store"]` or `["volume", "store"]` | cache tier stack (anyfs), top first: an ordered subset of `local`, `volume`, `store` ending in `store`, e.g. `["local", "volume", "store"]` (local disk caching a cache volume over the store). A stack with `volume` gets cache volumes (`medium` may be omitted; if set it must agree). The scheduler composes the tiers' profiles to estimate lazy restores (working set page-in), thrashing (working set beyond the top tier) and storage cost |

A provider without sizes gets one size `default` of `pool.machine_cpu` and
`pool.machine_memory`.

Cache volumes of `medium = "volume"` sizes are configured per provider in
`[providers.NAME.volume]` (see [caches and storage](caches-and-storage.md#volumes)).
Volumes are off by default (`medium = "local"`).

| volume key | default | |
|---|---|---|
| `type` | `gp3` | aws-ec2 volume type |
| `iops`, `throughput` | gp3 baseline (3000, 125 MiB/s) | aws-ec2 provisioned IOPS and MiB/s |
| `vpus` | 10 (balanced) | oci volume performance units per GB |
| `min` | the size's `disk`, else `100G` | size of a new volume |
| `max` | provider limit | largest size a volume grows to |
| `step` | `50G` | least growth of a resize (aws-ec2: one resize per volume every 6 h) |
| `keep_detached` | `false` | keep the volume when its machine is destroyed; a later machine of the provider reuses it (and is placed in its zone) |
| `ttl` | `24h` | a detached volume is deleted after this long; content unused for longer is wiped when it is attached |

Each medium has a capability profile with approximate defaults per provider
type (see [caches and storage](caches-and-storage.md#capability-profiles));
`[providers.NAME.media.MEDIUM]` (`MEDIUM` = `local`, `volume`, `store`)
overrides single values: `throughput` (per second, `"500M"`), `iops`,
`latency` (`"1ms"`), `price_per_gb_month`, `request_per_1k`,
`egress_per_gb`, `attach`, `provision` (durations), `max_size`,
`resize_cooldown`.

```toml
[providers.aws]
type = "aws-ec2"
[providers.aws.volume]
throughput = 250
keep_detached = true
[providers.aws.media.volume]
price_per_gb_month = 0.0952   # eu-central-1 gp3
[[providers.aws.size]]
name = "c7a.4xlarge"
cpu = 16
memory = "32G"
medium = "volume"
```

#### `local-docker`

Machines are privileged Docker containers on a Docker host (FUSE, cgroups).

| option | default | |
|---|---|---|
| `command` | `["docker"]` | Docker CLI invocation, e.g. `["sudo", "-n", "docker"]` |
| `host` | `127.0.0.1` | address at which published machine ports are reachable |
| `binary` | this executable | static binary mounted into machines, as a path on the Docker host |

#### `aws-ec2`

See the [AWS guide](guides/aws.md) for IAM, quotas and costs.

| option | default | |
|---|---|---|
| `profile` | default credential chain | `~/.aws/config` profile |
| `region` | the profile's region, else `eu-central-1` | |
| `stack` | `offshore` | name of the shared CloudFormation stack |
| `allow_cidrs` | embedded: the orchestrator's public IP (checkip.amazonaws.com), added to the existing rules; shared control plane: required | CIDRs of the clients that may reach machines (UDP 7443). An explicit list is authoritative: UDP 7443 rules for other sources are revoked. `["0.0.0.0/0"]` admits any client (sessions still need a lease token) |
| `subnet` | default VPC | subnet of the machines |
| `disk_gb` | 100 | gp3 root volume |
| `disk_iops`, `disk_throughput` | 6000, 500 (MiB/s) | provisioned gp3 performance of the root volume (3000..80000 IOPS, at most 500 per GiB; 125..2000 MiB/s, at most IOPS/4). gp3's baseline (3000/125) saturates with concurrent builds: cargo's writes, the slot hasher, page-ins and writeback share the volume (~$0.05/h per machine for the defaults) |
| `docker_command` | `["docker"]` | builds and pushes environment images |
| `ami`, `ami_arm64` | newest Amazon Linux 2023 | AMI override per architecture |
| `binary`, `binary_amd64`, `binary_arm64` | this executable, `offshore-linux-<arch>` next to it | worker binaries uploaded for the machines |

#### `oci`

See the [OCI guide](guides/oci.md).

| option | default | |
|---|---|---|
| `profile` | `DEFAULT` | `~/.oci/config` profile |
| `config_file` | `~/.oci/config` | |
| `region` | the profile's region | |
| `compartment` | `offshore` | name below the tenancy root (created) or an OCID |
| `allow_cidrs` | as for aws-ec2: required with a shared control plane, the orchestrator's public IP when embedded | clients that may reach machines (UDP 7443); an explicit list is reconciled on the security list (stale QUIC rules removed) |
| `subnet`, `availability_domain` | created VCN / first AD | |
| `image`, `image_arm64` | newest Oracle Linux 9 platform image | |
| `bucket` | `offshore` | Object Storage bucket (binaries, machine secrets, and the native store below `cache/<pool>/`, reached by workers through pre-authenticated requests) |
| `repository`, `registry` | `offshore-env`, the region's OCIR | environment image repository |
| `iam` | `true` | create the dynamic group and policy; `false` when the API user may not |
| `disk_gb` | 100 (minimum 50) | boot volume |
| `disk_vpus` | 20 | boot volume performance units per GB (10 balanced, 20 higher performance, 30..120 ultra high); throughput grows with VPUs and size (20 VPUs: 600 KB/s per GB, 120 MB/s at 200 GB), so a larger `disk_gb` is also faster |
| `docker_command` | `["docker"]` | |
| `binary`, `binary_amd64`, `binary_arm64` | as aws-ec2 | |

#### `static`

Machines you already run (bare metal, reserved instances) with sshd, Docker
and systemd. One machine per host; the orchestrator drives the host over
SSH and runs the supervisor there (`offshore supervisor static
apply|start|stop|destroy|status`).

```toml
[providers.lab]
type        = "static"
registry    = "registry.lab:5000/offshore-env"  # required: hosts pull environment images from here
ssh_key     = "file:~/.ssh/offshore"            # default file:~/.ssh/id_ed25519
known_hosts = "~/.ssh/known_hosts"                  # host keys are verified
hosts = [
  { name = "build1", ssh = "root@10.0.0.5", cpu = 32, memory = "128G" },
  { name = "build2", ssh = "root@10.0.0.6:2222", addr = "10.0.0.6:7443", cpu = 16, memory = "64G", arch = "arm64" },
]

[[providers.lab.size]]
name   = "lab"
cpu    = 16
memory = "64G"
```

| option | default | |
|---|---|---|
| `registry` | required | image repository the hosts are logged in to |
| `hosts` | required | `name` (default `ssh`), `ssh` (`user@host[:port]`), `addr` (default host:7443), `cpu`, `memory`, `arch` (default `amd64`) |
| `ssh_key` | `file:~/.ssh/id_ed25519` | reference to the private key |
| `known_hosts` | `~/.ssh/known_hosts` | |
| `docker` | `["docker"]` | local Docker CLI that builds and pushes images |
| `binary` | this executable | uploaded once to `/var/lib/offshore/bin/` |

The orchestrator places a machine on the first free host with enough CPU
and memory. At the end of a machine's lifetime the supervisor removes the
machine instead of powering the host off. Hosts must be reachable on UDP
7443. Static hosts have no native object store: configure `[storage]` for
snapshots and shared result caches.

## Repository configuration

`offshore init` generates a commented `.offshore.toml` from what the
repository declares (Cargo workspaces, `go.mod`, `package.json`, GitHub
Actions, Dockerfiles); see [cli.md](cli.md#init). A complete example:

```toml
version = 1

[resources]                 # per session: scheduling and cgroup limits
cpu        = 4              # default 2
memory     = "8G"           # default 4G; reservation, refined from measured peaks
max_memory = "16G"          # optional hard limit
arch       = "x86_64"       # x86_64 (default) | aarch64

[environment]               # built into a content addressed image
image  = "rust:1-slim-trixie"   # or: dockerfile = "ci/Dockerfile"
linker = "mold"             # optional: mold | lld

[environment.packages]
apt = ["pkg-config", "libssl-dev", "clang"]

[environment.toolchains]
rust = "rust-toolchain.toml"

[[environment.setup]]       # cached per hash of the previous layers, command and inputs
run    = "cargo install --locked cargo-nextest"
inputs = []

[environment.env]
CARGO_INCREMENTAL = "0"

[source]
default           = "watch"     # watch | immutable
respect_gitignore = true        # gitignored files are not projected

[[source.rule]]
paths = ["vendor/**"]
mode  = "immutable"             # watch | immutable | hidden

[[source.extra_root]]           # a directory outside the repository
path = "../shared-protos"
mode = "immutable"

[cache.target]
scope    = "slot"
paths    = ["target"]
prune    = "rust"
max_size = "100G"

[cache.deps]
scope = "machine"
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]

[cache.sccache]
scope = "global"
tool  = "sccache"

[services.postgres]             # sidecar, reachable on localhost in sessions
image   = "postgres:17"
env     = { POSTGRES_PASSWORD = "dev" }
port    = 5432
command = ["postgres", "-c", "fsync=off"]

[secrets]                       # resolved locally per run, never stored remotely
GH_TOKEN = "cmd:gh auth token"

[tool.cargo]
local = ["publish", "login", "install", "new", "init", "add", "remove"]
eager = ["target/debug/myapp"]
env   = ["RUSTFLAGS", "CARGO_TERM_*", "RUST_LOG", "RUST_BACKTRACE"]

[tool.npm]
ports = [8080, "3000:3001"]

[outputs]
mode = "eager"                  # eager (default) | fuse

[editor]
rust_analyzer = "local"         # local (default) | lsp-remote

[prewarm]
run = ["cargo", "check", "--workspace", "--tests"]
```

### `[resources]`

| key | default | |
|---|---|---|
| `cpu` | 2 | initial CPU estimate per session (the history estimator replaces it with measurements) |
| `memory` | `"4G"` | initial memory reservation |
| `max_memory` | none | hard memory limit of the session cgroup |
| `arch` | `"x86_64"` | `x86_64` or `aarch64`; only sizes of this architecture are created, and the environment image is built for it. Build outputs must run on the client, hence the default. |

### `[environment]`

The environment image is content addressed: `offshore env hash` prints
its hash, which covers the base image or Dockerfile directory, packages,
toolchain file, setup steps with their `inputs`, environment variables,
linker, services and the platform. When it changes, the daemon builds the
new image in the background (see [cli.md](cli.md#env)).

| key | |
|---|---|
| `image` | base image (`base = "image"`, the default without `dockerfile`) |
| `dockerfile` | path of a Dockerfile relative to the repository root (`base = "dockerfile"`) |
| `base` | `"image"` or `"dockerfile"`; derived from the other two keys |
| `packages.apt` | Debian/Ubuntu packages installed into the image |
| `toolchains.rust` | `rust-toolchain(.toml)` path; the image installs that toolchain |
| `[[setup]]` | `run` (shell command, required), `inputs` (repository files whose content keys the layer) |
| `env` | environment variables of every session |
| `linker` | `mold` (pinned static release) or `lld` (distribution package); see [caches-and-storage.md](caches-and-storage.md#linker) |

### `[source]`

Sources are projected onto the machine at the same absolute path.

| key | default | |
|---|---|---|
| `default` | `"watch"` | `watch` (changes are tracked and synchronised before each run) or `immutable` (read once, not watched) |
| `respect_gitignore` | `true` | files matched by `.gitignore` and `.git/info/exclude` are hidden |
| `[[rule]]` | | `paths` (globs), `mode` = `watch`, `immutable` or `hidden` |
| `[[extra_root]]` | | `path` (absolute or relative to the repository), `mode` = `watch` or `immutable` |

Slot and digest cache paths are always hidden from the projection.

### `[cache.<name>]`

Every table is one cache; names match `[a-z0-9_-]+`. Full semantics in
[caches-and-storage.md](caches-and-storage.md).

| key | scopes | default | |
|---|---|---|---|
| `scope` | all | `global` with `tool`, else `slot` | `slot`, `machine` or `global` |
| `paths` | slot, machine | required | slot: plain directories inside the repository; machine: `$VAR`, `~`, absolute or inside the repository |
| `digest` | machine | | files whose content keys an immutable cache (with `install`) |
| `install` | machine + digest | | command that generates the cache remotely |
| `tool` | global | required | `sccache`, `ccache` or `go` (each at most once) |
| `max_size` | slot, machine | unbounded | budget enforced by pruning |
| `prune` | slot, machine | `lru` (`none` with `digest`) | `rust`, `lru` or `none` |
| `ttl` | all | slot 24h, machine 7d, global 24h | global: at most 24h |

### `[tool.<name>]`

Per-tool configuration. A tool is proxied only when it has a shim (or runs
through `offshore run`).

| key | default | |
|---|---|---|
| `local` | none | subcommands that always run locally (`"run dev"` selects one node script) |
| `remote` | everything not in `local` | subcommands that run remotely |
| `env` | none | environment allowlist forwarded to remote runs (trailing `*` allowed) |
| `eager` | none | output globs copied back after every run |
| `ports` | none | local ports forwarded into the session: `8080` or `"local:remote"` |
| `adapter` | by tool name | `cargo`, `go`, `node`, `generic` |
| `split` | all the adapter supports | test subcommands built remotely and run locally (cargo: `"test"`, `"nextest"`; go: `"test"`); `[]` turns it off |

Adapters, the test split and port forwarding are described in
[tools.md](tools.md).

### `[services.<name>]`

Sidecar containers started next to every machine of the environment; they
share the machine's network namespace, so sessions reach them on
`localhost:<port>`. Names match `[a-z0-9-]+`. Keys: `image` (required),
`env`, `port`, `command`. Services are part of the environment hash, so
machines are shared only by repositories with the same services.

### `[secrets]`

`NAME = "env:VAR" | "file:PATH" | "cmd:COMMAND"`. Resolved on the laptop
for each run and passed to the session as environment variable `NAME`;
never written to disk remotely. `offshore doctor` resolves every
secret without printing it.

### `[outputs]`, `[editor]`, `[prewarm]`

| key | default | |
|---|---|---|
| `outputs.mode` | `"eager"` | `eager`: only `[tool.X] eager` outputs come back; `fuse`: the slot cache directories are mounted locally as a writable overlay ([tools.md](tools.md#output-overlay)); needs a slot cache |
| `editor.rust_analyzer` | `"local"` | `local` or `lsp-remote` ([tools.md](tools.md#rust-analyzer)) |
| `prewarm.run` | none | warm-up command of `offshore prewarm` ([operations.md](operations.md#prewarm)) |
