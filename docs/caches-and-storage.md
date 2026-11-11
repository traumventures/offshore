# Caches and storage

Build state lives in three places, by scope:

| scope | lives | keys |
| --- | --- | --- |
| `slot` | in the session's exclusive build slot on the machine, reused warm by the next session of the repository | `paths` (plain directories inside the repository), `ttl` (default 24h), `max_size`, `prune` |
| `machine` | in the machine cache directory, shared by all sessions of the machine | `paths` (`$VAR`, `~`, absolute or inside the repository), `ttl` (default 7d), `max_size`, `prune`; with `digest` + `install`: generated per lockfile |
| `global` | in the pool's object store, shared by all machines | `tool` (`sccache`, `ccache`, `go`), `ttl` (at most 24h) |

Without `scope`, a table with `tool` is global, otherwise slot. Slot and
machine paths inside the repository are hidden from the source projection
(they never come from the laptop). Machine-cache `$VAR` paths are expanded
with the session's environment (the image's `CARGO_HOME`, `GOPATH`, ...).

```toml
[cache.target]
scope    = "slot"           # worker-local build slot, never synchronised
paths    = ["target"]
ttl      = "24h"            # per build slot, capped by the pool's max_lifetime
max_size = "100G"           # slot budget, enforced by pruning
prune    = "rust"           # rust | lru (default) | none

[cache.deps]
scope = "machine"           # shared by the sessions of a machine
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]
ttl   = "7d"

[cache.node-modules]
scope   = "machine"
paths   = ["web/node_modules"]                # generated remotely, hidden from the projection
digest  = ["web/package.json", "web/bun.lock"] # immutable per content hash (+ platform)
install = "cd web && bun install --frozen-lockfile"

[cache.sccache]
scope = "global"
tool  = "sccache"           # sccache | ccache | go; installs the tool if needed
ttl   = "12h"               # optional, at most 24h
```

## Build slots

A session leases a slot of its repository exclusively, so parallel builds
of one repository never block each other. Slots are reused warm and never
older than the shortest slot-cache `ttl` (default 24 h, recorded per slot,
so repositories with different TTLs share machines). New slots are cloned
by reflink when the file system supports it, otherwise created cold.
Machines live at most `max_lifetime`, which also caps the slot TTL. The
scheduler prefers machines that hold a warm slot of the repository
([scheduling.md](scheduling.md)).

## Machine caches per digest (`node_modules`)

A machine cache with `digest` and `install` is immutable per content hash:
the digest covers the matching files (globs, e.g. the lockfile), the cache's
name, paths and install command, and the environment's platform
(`os/arch/libc`). The first session that needs a digest runs `install`
remotely (sandboxed, with the session's environment and the cache paths
bound to a fresh directory; its output appears on stderr) and stores the
result in the machine store `<deps dir>/digest/<name>/<digest>`. Every slot
gets a copy linked from the store on the same disk: reflinks where the file
system supports them (writable), otherwise hard links mounted read-only, so
sessions can never change the store. The copy is refreshed when the digest
changes. The paths are hidden from the source projection, so a local
`node_modules` (e.g. for another platform) never reaches the machine.
Digests unused for the cache's `ttl` are removed, and per cache the least
recently used ones beyond `max_size` (the newest stays). Plain machine
caches with `max_size` are pruned by least recent access while the machine
is idle. With an object store, digests are also saved to
`envs/<env>/digests/<name>/<digest>/` after their install, and other
machines restore them instead of running the install.

## Pruning

Slots are pruned against two budgets: per slot the sum of the slot caches'
`max_size` (unset: unbounded) after every session, per machine
`[scheduler] disk_budget` of the cache file system (default `"90%"`, or a
size like `"150G"`; a size's own `disk_budget` overrides it). The
orchestrator passes it to each new machine. The worker tracks reads per
session: after a run it records for every slot file the last run (session
number of the slot) that read or wrote it, using the atime (relatime
re-armed by resetting the atime of read files to their mtime; on `noatime`
mounts only writes count). Pruning runs in stages until the slot fits:

1. `prune = "rust"`: old incremental compilation sessions (all but the
   newest per crate in `target/*/incremental/`);
2. units not read for 5 runs, the stalest first (`rust`: all files of one
   compilation unit, i.e. `deps/*-<hash>.*`, `.fingerprint/<unit>/`,
   `build/<unit>/`; `lru`: single files); with a size target also more
   recently read ones, never those of the latest run;
3. the whole slot (cold; the next session starts a new one).

`prune = "none"` leaves only the TTL and the machine budget. Over the
machine budget the least recently used slots get the policy prune (stages 1
and 2) first, then go cold. The orchestrator can prune a repository's slots
on demand through the worker's slot stream (`proto.KindSlot`, admin token;
`orchestrator.WorkerSlotOps.PruneSlot`): older duplicate slots go cold
first, then the newest is pruned in stages to the requested size.

## Object store (`[storage]`)

Internally a provider has four parts: **Compute** (machines), **Registry**
(environment images), **Store** (an S3-compatible object store for slot
snapshots and global caches) and **Secrets** (the channel that hands a new
machine its secret environment). The pool's `[storage]` section replaces
the Store part of every provider:

```toml
[storage]
endpoint   = "https://<account>.r2.cloudflarestorage.com"   # "" = AWS S3
bucket     = "offshore-cache"
region     = "auto"
prefix     = "cache/team"           # default "cache/<pool id>"
access_key = "env:R2_ACCESS_KEY_ID" # env:/file:/cmd: references
secret_key = "env:R2_SECRET_ACCESS_KEY"
# role_arn   = "arn:aws:iam::123:role/cache"  # STS: per-repository credentials
# path_style = true                            # MinIO
# dir        = "/srv/offshore-store"       # a directory every machine mounts (instead of bucket)
```

| key | |
|---|---|
| `endpoint` | S3 endpoint URL; empty = AWS S3 in `region` |
| `bucket` | bucket name (required unless `dir`) |
| `region` | region of the bucket |
| `prefix` | key prefix, default `cache/<pool id>` |
| `access_key`, `secret_key` | references to the control plane's credentials (both or neither) |
| `role_arn` | STS role from which per-repository credentials are minted; required with a bucket unless keys are given |
| `path_style` | path-style addressing |
| `dir` | directory store shared by the machines (exclusive with `bucket`) |

Native stores: `aws-ec2` uses the stack's bucket below `cache/<pool>/`
(one-day lifecycle) and the stack's `StoreRole`; `local-docker` a volume
`cs-store-<pool>` shared by its containers (`/store`); `oci` its bucket
below `cache/<pool>/` (one-day lifecycle) through pre-authenticated
requests (below); `static` has none (use `[storage]`). Without a store, global caches stay machine-local and slots
cannot move between machines.

The control plane never moves cache data. With every lease (and every slot
save or restore) it hands the worker a **grant**: credentials scoped to the
repository's prefix (`repos/<repo id>/`) and the environment's
(`envs/<env hash>/`), at most 12 h. With `role_arn` (and on `aws-ec2`) they
come from STS AssumeRole with a session policy limited to those prefixes;
otherwise the configured keys are handed out as they are, so give them only
the bucket (or prefix) they need. On `oci` the grant holds no credentials
but one pre-authenticated request URL per prefix (read/write below it,
no listing, no delete), see the [OCI guide](guides/oci.md#store-the-bucket-through-pre-authenticated-requests). Objects are build caches and never served
after 24 h.

## Global result caches and the compiler cache

Global caches use a tiered store in the worker: a directory on the machine
disk, backed by the grant's environment prefix, so a fresh machine reuses
what earlier machines compiled. Keys are prefixed with the environment hash,
so toolchains never mix. A `ttl` below 24h lowers the age of reused entries.

* `sccache`: the pinned static sccache release (checksum verified,
  downloaded once on the client into the user cache directory,
  `OFFSHORE_TOOL_CACHE` overrides) is added to the environment image.
  Each session gets its own sccache server on a unix socket inside the
  session sandbox (`SCCACHE_SERVER_UDS`, `SCCACHE_DIR` in a private
  temporary directory); `RUSTC_WRAPPER` is set unless the user set it. The
  backend is WebDAV (`SCCACHE_WEBDAV_ENDPOINT`) served by the worker on
  `127.0.0.1` (`internal/blobcache`) from `<deps dir>/sccache`
  (`OFFSHORE_SCCACHE_DIR`) backed by the store.
* `ccache`: installs the Debian package and points `CCACHE_REMOTE_STORAGE`
  at the endpoint (local `CCACHE_DIR` on the machine disk).
* `go`: sets `GOCACHEPROG` to `offshore gocacheprog`, which keeps
  outputs on the machine disk (go reads them from there) and shares them
  through the endpoint.

sccache cannot cache incremental compilations, which cargo uses for
workspace crates in dev profiles; dependencies are cached regardless. Set
`CARGO_INCREMENTAL=0` in `[environment.env]` to make them cacheable. With
`OFFSHORE_VERBOSE=1` the proxy prints the run's statistics
(`sccache: 3 hits / 1 misses (...)`); they are also part of the session's
exit message (`proto.Exit.Cache`) and shown by `offshore why`.

### Linker

`[environment] linker = "mold"` installs the pinned static mold release
(`lld` installs the distribution's `lld` package, Debian based images) and a
driver `/usr/local/bin/offshore-cc-<linker>` (`cc ... -fuse-ld=<linker>`,
last so it beats rustc's own `-fuse-ld=lld` for its bundled rust-lld), which
sessions use as `CARGO_TARGET_<host triple>_LINKER` unless set (with
`[outputs] mode = "fuse"` through a `cc` on `PATH` instead, see
[tools.md](tools.md#cargo-freshness)). `RUSTFLAGS` and `rustflags` from
`.cargo/config.toml` stay untouched; a `target.<triple>.linker` in
`.cargo/config.toml` is overridden by it.

## Snapshots and lazy restore

A build slot (and a machine cache per digest) can move between machines
through the store. `internal/snapshot` saves a directory as content
addressed chunks (BLAKE3; small files packed into ~8 MB chunks with
content-defined boundaries, large files split) plus a manifest with every
file's mode, size, mtime and chunk ranges
(`repos/<repo>/snapshots/<unix>.json.gz`, newest named by
`snapshots/latest`). A save reuses the chunk ranges of the slot's previous
save for files whose size and mtime did not change, so it reads and uploads
only what changed; chunks older than 12 h are uploaded again, so a snapshot
stays restorable although the store expires objects after 24 h.

A restore creates a new slot at once: directories, symlinks and sparse
placeholders with the final sizes and the original mtimes (cargo compares
mtimes, so a restored `target/` is fresh). File contents are filled on
first access through a FUSE fill layer mounted on the slot's data directory
(a loopback that fetches a file before it is opened, truncated or renamed)
and by a background fill; once complete and not in use, the mount is
replaced by the filled directory. Without FUSE the restore fills everything
before it returns. The slot keeps its original creation time, so the 24 h
bound holds across machines.

The scheduler decides when (`orchestrator.SlotOps`): `SaveSlot` and
`RestoreSlot` call the worker's slot stream with a fresh grant; the first
session on a restored slot reports `usage.restored` and
`usage.restore_seconds`. A worker draining at the end of its lifetime saves
the slots of every repository it holds a grant for and lists them in
`Status.Snapshots`. Events: `snapshot_save`, `snapshot_restore`
([operations.md](operations.md#event-log-and-reports)).

Measured with `test/bench/snapshot.sh` (local directory store, small crate
with serde, serde_json, itoa, libc):

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

## Disks

`aws-ec2` implements `provider.DiskResizer`: the scheduler can grow a full
machine's gp3 root volume (`ec2:ModifyVolume`, once per six hours, never
shrinking) and the supervisor grows partition and file system
(`growpart`, `xfs_growfs`/`resize2fs`) on its next check. Sizes may declare
`disk = "200G"` so the scheduler plans slots against it.

### Volumes

Providers expose attachable block volumes as their `Volumes` part
(`internal/provider/volumes.go`): create, attach, detach (with an expiry
for reuse), grow, delete and list by tag. Volumes outlive machines and
attach only within their zone.

| provider | volume | zone | attach | resize |
|---|---|---|---|---|
| aws-ec2 | encrypted EBS (gp3, `iops`/`throughput`), 1 GiB..16 TiB | availability zone | running or stopped instances | grow, once per 6 h |
| oci | block volume (performance units per GB), 50 GB..32 TB | availability domain | paravirtualized, `/dev/oracleoci/oraclevdb` | grow, online |
| local-docker | Docker volume (unbounded directory) | `local` | at machine creation only | not needed |
| static | none: the host's fixed disk | | | |

A machine created with `MachineSpec.Volume` is placed in the volume's
zone and gets it attached. The reaper deletes detached volumes past their
expiry and cache volumes whose machine is gone and that carry no expiry.
How the orchestrator and scheduler use volumes (medium per size, pool,
TTL) is being designed.

Machines use gp3 root volumes (`disk_gb`, default 100; 3000 IOPS and
125 MB/s baseline). Instance types with local NVMe (`c6id`, `m6id`,
`c7gd`) are faster for target directories but lose their content on stop,
so their warm state only survives through snapshots. To compare on a real
machine:

1. Start one machine per disk type (e.g. sizes on `c6i.2xlarge` and
   `c6id.2xlarge`); for NVMe mount the instance store at the cache dir.
2. Raw disk: `fio --name=rand --rw=randrw --bs=16k --size=4G --numjobs=8
   --iodepth=32 --direct=1 --runtime=60 --group_reporting --filename=/cache/fio`
   and `--rw=write --bs=1M` for sequential throughput.
3. Builds: run the same `cargo build` cold, with warm sccache and warm slot
   on each, and compare `task_end.usage` (`wall`, `cpu_stall_seconds`) with
   `offshore report`.
4. Restore: `test/bench/snapshot.sh` with `BENCH_STORE` on the machine
   measures save and fill against each disk.

## Lifetimes

Every layer that holds build artifacts is bounded; see
[operations.md](operations.md#24-h-guarantees-and-cleanup) for the full
table.
