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
of one repository never block each other. A slot is a mount of the cache
file system (see [anyfs](#anyfs-one-storage-core) and
[build slots on anyfs](#build-slots-on-anyfs-commit-writeback-lazy-restore)):
a private upper directory over the repository's committed layers, so a
second slot of the same repository is another overlay over the same
layers, not a copy. Slots are never older than the shortest slot-cache
`ttl` (default 24 h, measured from the oldest data in the layer chain,
recorded per slot, so repositories with different TTLs share machines).
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

### Access classes

The cache file system learns how a slot's files are used
(`internal/access`). Every path maps to a pattern with hashed and numbered
segments generalized (`target/debug/deps/*.rlib`,
`target/debug/incremental/*/s-*/dep-graph.bin`,
`target/debug/.fingerprint/*/*.json`). Per pattern and across sessions the
worker counts creates, overwrites, deletes and reads (from the trees
before and after each session and the read index), lifetimes and sizes,
persisted as a small JSON file per repository, and classifies:

| class | means | policy |
| --- | --- | --- |
| `immutable` | read-mostly, (almost) never written | upload early, prefetch after a restore, reclaimed last, deduplicated across slots |
| `write-once` | new files most sessions, never overwritten | upload after 5 s, prefetch, reclaimed last, deduplicated |
| `churn` | overwritten or replaced most sessions | stays local (never paged out to a volume), dirty for 10 min, pages of files deleted before the flush are skipped, reclaimed early |
| `ephemeral` | created and deleted within about a session, never read | never written back, reclaimed first |

Tool adapters seed the classifier with priors (cargo: `deps/` immutable,
`incremental/` and `.fingerprint/` churn, `build/` write-once,
`target/tmp` ephemeral; go: module cache immutable, build cache
write-once; node: `node_modules` immutable, bundler caches churn). A
pattern's own statistics override the prior once it was seen in 3
sessions; unseen files fall back to their directory's statistics.

On workers the slot file system feeds every commit into the repository's
model (`<cache>/anyfs/repos/<repo>/access.json`) together with the files
sessions read since the last commit.

**Reads and the working set.** Reads are tracked per session without
atime: on the kernel overlay with fanotify on the slot's overlay super
block (needs CAP_SYS_ADMIN, which the kernel overlay needs anyway; it also
sees the session's bind mounts in its own mount namespace, whose paths are
mapped back through the session's binds), on the userspace overlay through
its open callback; the worker's own accesses are filtered out. Plain
directory slots do not track reads (the read index of the prune policies
falls back to atime there). Each session's reads also update the
repository's **working set** (`workingset.json`: per path the last session
that read it, how often, its size; entries unread for 16 sessions are
dropped). It is carried in the meta of every committed layer (top 8000
entries), so it travels with the head: a slot mounted fresh over a head
(restore, move, a new slot) reads its working set ahead, most recently
read first, then the class prefetch, within the prefetch budget.
`working_set_bytes` (`Status.VM`, metric) is the size of the files read in
the last 3 sessions; a session's `usage.working_set_bytes` the bytes it
read plus what the slot grew (fault bytes without tracking). Two policies act
today: reclaim drops materialized files of ephemeral, then churn files
before the least recently used rest, and a slot mounted fresh over a
committed head prefetches its immutable and write-once files (larger than
the small files prefetched anyway, up to 2 GiB or a quarter of the
capacity).

The commit also applies the rest of the table to the pages it creates:

* **Writeback delay per class.** Each page carries its class's hint
  (`anyfs.PageHint`); the repository's link to the store writes dirty
  pages back in the background when their delay expires (immutable at the
  next pass, write-once after 5 s, unknown after 30 s, churn after 10 min,
  ephemeral never by itself). Pages of files deleted before that are
  discarded unwritten at the next commit. An idle commit only moves the
  store head (layer objects + `heads/slot`, cheap) once all pages of the
  head are clean, or flushes the head whole after 10 min (the most
  committed work that lives only on the machine); save, move, drain,
  SIGTERM and a session over the dirty threshold flush right away.
* **Placement.** Churn and ephemeral pages are local-only: an exclusive
  link (local over a volume) never demotes them on reclaim; they reach the
  store only as part of a committed layer.
* **Dedupe.** Immutable and write-once files from 16 KiB get a page of
  their own whose key is their content digest (smaller files stay packed),
  so the same file in another slot or repository is the same page. The
  repositories' local page tiers share a machine-wide pool
  (`<cache>/anyfs/pool`) by hardlink: a page is on disk once
  (`shared_bytes` is subtracted from the reported residency), and once per
  store prefix (a store put skips young existing objects). Repositories do
  not share a store prefix: the prefix is the grant's security scope.

Page hints live in memory: after a worker restart dirty pages use the
default 30 s delay.

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

## anyfs: one storage core

Build slots on workers, the source projection on workers and the output
overlay on laptops are configurations of one storage core,
`internal/anyfs`:

- **Tree** (manifest): every path with kind, mode, size, mtime, the BLAKE3
  digest of its content and the **pages** holding it. Pages (about 8 MiB)
  are content addressed; small files (up to 1 MiB) are packed together with
  content defined boundaries, large files split.
- **Layer**: a committed delta of a tree (changed nodes, whiteouts, opaque
  directories) with a parent; a chain of layers folds into a tree,
  compaction folds a chain into a new base. Layer objects and named
  **heads** live next to the pages in the store.
- **Tier**: stores pages (`Get`, `Put`, `Has`, `Reclaim`, `Profile`): the
  local disk, a volume, the object store, a peer over QUIC.
  `Link(upper, lower)` makes a pair of tiers a tier again, so stacks fold
  recursively, like a CPU cache hierarchy: `local -> volume -> store` on a
  worker with a cache volume, `local -> store` by default, the laptop's
  `local -> peer (the remote slot)` and the worker's source
  `local -> peer (the laptop)`.
- **Files** materializes node contents as whole local read-only files
  (shared per digest), the page cache the FUSE lower hands to the kernel.

Each link has a policy:

| policy | values | default |
| --- | --- | --- |
| inclusion | inclusive (pages faulted in stay below too) or exclusive (victim cache: faulted pages move up, reclaimed pages are demoted) | inclusive; exclusive for `local` over `volume` |
| write | write-through or write-back | write-back: new pages stay dirty locally until `[writeback] dirty_expire` or `dirty_ratio` |
| readahead | concurrent background page-ins of `Prefetch` | 8 |
| reclaim | ARC: pages touched once are reclaimed before pages touched again, so a clean build streaming through new pages does not evict the hot set; ghost entries adapt the balance and detect refaults | always |
| promote | keep faulted pages in the upper tier | on |

**Virtual memory model.** The local disk is RAM, lower tiers are swap and
the store is the backing store. Names in code, events and metrics follow
it:

| term | meaning |
| --- | --- |
| page | the unit of transfer and caching (a chunk of the store) |
| resident | held by the local tier (pages and materialized files) |
| clean / dirty | held below too (reclaim is free) / must be written back first |
| page fault | a lookup missed the upper tier; content is fetched (page-in) from below |
| writeback / page-out | dirty pages written to the tier below (a commit) / pages demoted |
| reclaim | resident clean pages dropped under pressure (faulted in again on use) |
| pin | a page that is never reclaimed |
| working set | bytes touched within a window (10 min) |
| thrashing | the working set exceeds the local capacity and recently reclaimed pages fault back in; the scheduler answers with a bigger disk or machine |

Events: `page_fault`, `page_in`, `reclaim` (per machine, deltas between
samples), `writeback` (per committed head), `thrash` (when a machine starts
thrashing); `page_out` is defined for demotions to a volume. Metrics:
`offshore_cache_resident_bytes`, `_dirty_bytes`, `_working_set_bytes`,
`_fault_rate`, `_reclaim_bytes`, `_thrashing`; `offshore top` shows
resident/capacity, dirty, working set, faults/s and `THRASH` per machine.

### Mount layouts and kernel requirements

| where | layout | needs |
| --- | --- | --- |
| worker build slot | kernel overlayfs (`redirect_dir=on,metacopy=on`): upper on the cache disk over a local metadata layer (metacopy files redirecting to content digests) and two data-only layers: the file cache, then a read-only FUSE digest view (passthrough) for content that is not resident | `CAP_SYS_ADMIN` in the initial user namespace (machine containers run with `--cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor=unconfined`), FUSE passthrough needs Linux >= 6.9 (without it reads go through the daemon), upper/work and the file cache on a real file system (the cache volume, not the container's overlay root) |
| worker build slot, unprivileged | userspace overlay (FUSE) over the same lower | `/dev/fuse` |
| worker without FUSE | plain directory; commits scan it, a slot over a chain is extracted eagerly | nothing |
| worker source projection | read-write anyfs FUSE (`WritableFS`) over the synced manifest, contents fetched per digest from the laptop (cached, LRU reclaim beyond budget/8, never open files); clean files by passthrough (inode per path + digest); local writes are written back with conflict detection | `/dev/fuse`; passthrough needs root and Linux >= 6.9 |
| laptop output overlay (`target/`) | userspace overlay: upper in the client state, whiteouts, newest-mtime merge over the remote slot (lazy listings over QUIC, contents in the client CAS) | `/dev/fuse` (no privileges) |

Why these choices (measured in `test/bench/tiers`): with passthrough no
read or write reaches the daemon; writeback cache would disable
passthrough, so it is never requested; flush, xattr and statx requests are
answered with ENOSYS once (halves the metadata round trips); the lower
fetches on the first read, never on open (overlayfs copy-up opens lower
files for their attributes); `metacopy` makes cargo's re-hardlinking of
uplifted artifacts metadata only (restore 0.4 s instead of 6-9 s);
`redirect_dir` lets rustc rename incremental directories from the lower.
A read-write FUSE costs +15-30 % on incremental builds, so build slots use
the kernel overlay wherever possible. Live files are never hardlinked into
the file cache (cargo writes fingerprints in place): commits copy or
reflink.

## Build slots on anyfs: commit, writeback, lazy restore

Build slots are mounts of the cache file system (`internal/anyfs`,
`internal/worker/cachefs.go`). A slot's data directory (what slot caches
bind into sessions) is a kernel overlayfs (`redirect_dir=on,metacopy=on`):
a slot-local upper directory over the tree of the repository's committed
**layer chain**, stacked as three lower layers (`anyfs.MountSlot`): a
local **metadata layer** (`<slot>/meta`, written at mount, ~0.1 s per 6k
nodes: directories, symlinks, and every file as a sparse metacopy file
with mode, size and mtime whose `trusted.overlay.redirect` names its
content `/ab/<digest>`), then as data-only layers the file cache and a
read-only FUSE **digest view** (`<slot>/lower`). overlayfs looks a file's
content up in the data-only layers in order: resident content is a local
file (no FUSE round trip for lookups, opens or reads), only misses reach
the daemon, which faults the content in. A file reclaimed from the cache
falls through to the digest view on its next lookup. File contents are
materialized on first read (never on open) by a machine-wide file cache
(`<cache>/anyfs/files`) from **pages** (content addressed, small files
packed) of the repository's tier stack: a local page tier
(`<cache>/anyfs/repos/<repo>/local`, ARC, capacity half the disk budget)
over the repository's store prefix (`repos/<repo>/` of the store grant).
Files faulted in through the digest view reach the kernel with FUSE
passthrough; files up to 64 KiB are prefetched at mount. Incremental
builds on a warm slot cost about the same as on the plain cache disk
(xfs: leaf/core within a few percent, see `test/bench/tiers/README.md`;
an ext4 upper is slower because ext4 flushes a file renamed over an
existing inode, and overlay turns deletions of lower files into
whiteouts that rustc's output renames replace). Without CAP_SYS_ADMIN the userspace overlay serves
the same union (logged at start); without `/dev/fuse` slots are plain
directories (commits scan them, a new slot over a chain is extracted).

**Commit = writeback.** A commit reads only what changed since the last
one (`anyfs.ScanChanges`): the entries of the overlay's upper directory
whose ctime is newer than the head's watermark (`scanned_ns` in the slot
metadata) are stat'ed in the merged view, their directories listed
(deletions, renames), whiteouts and opaque or redirected directories
rescanned; everything else is taken from the head tree unread. Plain
directory slots are walked whole but only files with a newer ctime are
read. In a test slot of 213 entries a second commit stats 5 entries and
hashes the 2 changed files (plain: stats 213, hashes 2). Changed files
become dirty pages and are committed as a layer (changed nodes + whiteouts) on top of the chain;
the layer's pages are flushed to the store, the layer objects and then the
head (`heads/slot`) written. Commits run when a slot has been idle for
`[writeback] dirty_expire` (default 30 s), right after a session that grew
the upper by more than `dirty_ratio` (default 0.1) of the disk budget, on a
save, on drain and on SIGTERM. Pages of files deleted before a flush are
discarded unwritten. Chains deeper than 16 layers, or whose base is older
than 12 h, are compacted into a new base, which also rewrites pages older
than 12 h (the store expires objects after 24 h; a chain's age is the age
of its oldest data, the slot TTL). With reflinks, committed upper files are
cloned into the file cache.

**Every session is committed.** When a session on an overlay slot ends,
before the slot is released, its changes are planned (stats only) and the
changed files captured into `slot/snaps/<ns>/` by reflink (FICLONE,
metadata only; a copy without reflinks, skipped above 256 MiB in favour
of the idle commit); the plan is written last. The slot is released at
once and the snapshot committed in the background (hashing, pages, layer,
head; the captured files move into the file cache) while the next session
may already run, so a busy slot's writeback flushes the previous session.
A commit from the live slot commits pending snapshots first; on start the
worker commits complete snapshots and drops incomplete ones before
serving: a crash loses at most the session that was running.

**Concurrent sessions** of a repository get another overlay over the same
committed head (no copy). A **rebase** (fresh upper over the new head; the
committed upper files move into the file cache) runs on idle slots whose
upper exceeds 4 GiB and under disk pressure. **Reclaim** under the disk
budget drops materialized files and clean pages first (faulted in again on
use), then rebases idle slots, and only then falls back to the prune
policies and cold slots. The working set, fault rate, dirty bytes and
thrashing (refaults while the working set exceeds the local capacity) are
reported as `Status.VM` (metrics `offshore_cache_resident_bytes`,
`_dirty_bytes`, `_working_set_bytes`, `_fault_rate`, `_reclaim_bytes`,
`_thrashing`; `machine_sample.vm`, `thrash` events).

**Save, restore, move.** `orchestrator.SlotOps.SaveSlot` commits and
flushes the slot and returns its head (`proto.LayerHead`: layer, chain,
bytes); `RestoreSlot` mounts a head on the target machine, which is
instant: contents fault in from the store on use. The first session on a
restored slot reports `usage.restored` and `usage.restore_seconds`. Saves
happen before the orchestrator stops or destroys a machine (slots changed
since the worker's last flushed head), for a **move** (save on the source,
mount on the target), and by the worker on drain and SIGTERM. Workers report
flushed heads in `Status.Heads`; the orchestrator logs a `writeback` event
per new head and keeps per repository the newest heads with their chains
(`layers.json` in its state dir). Its garbage collection (every 10 min)
deletes layer objects no kept head reaches and those older than 24 h;
pages expire with the store TTL. Machine caches per digest are shared
through the environment's store prefix as a single layer
(`digests/<name>/<digest>/`).

## Media: local, volume, store

Independently of a cache's scope, a machine's caches live on one of three
tiers. The object store is the durable source of truth; the live disk is a
hot cache of it (builds read and write only there).

| tier | medium | what | speed | survives | moves by | cost |
| --- | --- | --- | --- | --- | --- | --- |
| hot | `local` (default) | root disk, or instance-store NVMe (aws-ec2 `*d` types, oci DenseIO) | fastest (NVMe: GB/s) | nothing beyond the machine | snapshot to the store + lazy restore | root disk: per GB; NVMe: in the instance price |
| hot / warm | `volume` | attachable block volume (EBS gp3, OCI block volume) | network disk (gp3 125 MiB/s baseline) | the machine (detached, until `ttl`) | detach/attach within its zone; other zone or provider: snapshot path | per GB-month, also while detached |
| cold / durable | `store` | S3-compatible object store (`[storage]` or the provider's) | network, tens of ms first byte | everything; 24 h TTL | free (reachable everywhere) | per GB-month + requests + egress |

The medium is chosen per size (`medium = "local" | "volume"`); volumes are
off by default until measurements justify them. Repository caches
(`[cache.<name>]` scopes) do not care about the medium.

* **local, instance store**: the supervisor finds the instance's NVMe
  disk (`/dev/disk/by-id/nvme-Amazon_EC2_NVMe_Instance_Storage_*`,
  `/dev/nvme*n1` on oci), formats it (xfs with reflink) once per boot
  (it is blank after every stop/start) and mounts it at the cache
  directory. aws-ec2 takes the size's disk from the instance type.
* **volume**: the orchestrator creates an encrypted volume
  (`[providers.NAME.volume]`: `min` size, `type`/`iops`/`throughput`,
  `vpus`) or reuses a detached one, places the machine in its zone and
  attaches it; the supervisor formats it once (xfs with reflink), mounts
  it at the cache directory and wipes content unused for longer than
  `ttl` when it is attached. Full volumes grow by at least `step` up to
  `max` (aws-ec2: once per 6 h); the supervisor rescans the device and
  grows the file system online. When the machine is destroyed the volume
  is deleted, or with `keep_detached = true` kept detached until `ttl`;
  the scheduler then creates the next machine of a volume size on the
  warm volume when one holds a slot of the waiting repository.
  State: `volumes.json` in the orchestrator state; events `volume_create`,
  `volume_attach`, `volume_detach`, `volume_resize`, `volume_delete`
  (with `bytes`, and `cost`: the storage cost since the volume's previous
  event).
* **store**: build slot layers and pages (content addressed) and global result
  caches; see below.

### Capability profiles

Every medium of every provider has a capability profile
(`internal/medium`): throughput, IOPS, latency, reach (machine, zone,
region, global), durability, price per GB-month, request and egress
prices, attach and provision time, size limits and resize cooldown. The
scheduler sees one composite profile per machine (hot medium over the
store) and uses it for mobility (moving a warm slot: local means a
snapshot and a restore bounded by the slower of store and hot medium; a
detached volume in its zone means the attach time), storage cost (volume
sizes pay their volume per hour, also while stopped or detached) and
resizes (step, cooldown, price). Code defaults are approximate list values
with the provider documentation cited in `internal/medium/defaults.go`:

| provider | medium | throughput | IOPS | price/GB-month | attach | limits |
| --- | --- | --- | --- | --- | --- | --- |
| aws-ec2 | local (gp3 root) | 125 MiB/s | 3000 | 0.08 | | 16 TiB, resize every 6 h |
| aws-ec2 | local (instance store) | ~1.5 GB/s | ~100k | in the instance | | lost on stop |
| aws-ec2 | volume (gp3) | 125 MiB/s (up to 1000) | 3000 (up to 16000) | 0.08 | ~10 s | 1 GiB..16 TiB, resize every 6 h |
| aws-ec2 | store (S3) | ~200 MB/s | | 0.023 (+0.0004 per 1000 GET) | | egress ~0.09/GB |
| oci | local (boot, balanced) | ~48 MB/s per 100 GB | 60/GB | 0.0425 | | 32 TB |
| oci | local (DenseIO NVMe) | ~2 GB/s | ~200k | in the shape | | lost on terminate |
| oci | volume (balanced) | 480 KB/s per GB | 60/GB | 0.0425 | ~45 s | 50 GB..32 TB, online resize |
| oci | store | ~200 MB/s | | 0.0255 | | 10 TB/month egress free |
| local-docker, static | local / volume | host disk | | 0 | ~1 s | |

Override values per provider and medium with
`[providers.NAME.profile.MEDIUM]` (see
[configuration](configuration.md#providers)); measured values override
both through `medium.SetMeasured`: workers count the transfers of their
cache file system per medium (page-ins as reads, writebacks and page-outs
as writes; request size and time) and report the cumulative counters in
their status (`tiers`). The orchestrator turns the deltas per machine into
exponentially decayed sums per provider instance and medium (time
constant one hour), fits time = latency + bytes / throughput over the
requests and, after 16 requests, uses the result as the measured
throughput and latency (profile `source = "measured"`) of the sizes the
scheduler sees and of `medium.Resolve`.

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

A machine created with a volume is placed in the volume's zone and gets it
attached. The reaper deletes detached volumes past their expiry, managed
volumes whose machine disappeared are released like destroyed ones, and
unmanaged cache volumes whose machine is gone and that carry no expiry are
deleted.

Machines use gp3 root volumes (`disk_gb`, default 100; 3000 IOPS and
125 MB/s baseline). Instance types with local NVMe (`c6id`, `m6id`,
`c7gd`) are faster for target directories but lose their content on stop,
so their warm state only survives through committed layers in the store. To compare on a real
machine:

1. Start one machine per disk type (e.g. sizes on `c6i.2xlarge` and
   `c6id.2xlarge`); for NVMe mount the instance store at the cache dir.
2. Raw disk: `fio --name=rand --rw=randrw --bs=16k --size=4G --numjobs=8
   --iodepth=32 --direct=1 --runtime=60 --group_reporting --filename=/cache/fio`
   and `--rw=write --bs=1M` for sequential throughput.
3. Builds: run the same `cargo build` cold, with warm sccache and warm slot
   on each, and compare `task_end.usage` (`wall`, `cpu_stall_seconds`) with
   `offshore report`.
4. Restore: `offshore` slot save on one machine and restore on the other
   (`snapshot_save`/`snapshot_restore` events) plus `test/bench/tiers`
   (layouts E and F use the real anyfs implementation)
   for the fault path against each disk.

## Lifetimes

Every layer that holds build artifacts is bounded; see
[operations.md](operations.md#24-h-guarantees-and-cleanup) for the full
table.
