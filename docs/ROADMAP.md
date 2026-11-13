# Roadmap

Status as of November 2026.

## Done

* Transparent proxy (shims, `offshore run`), PTY, signals, stdin, exit
  codes, local fallback, one retry on machine loss.
* Source projection over FUSE at the same path, barrier per run, write-back
  with conflict detection, `watch`/`immutable`/`hidden` rules.
* Content addressed environment images (image or Dockerfile, apt,
  toolchains, setup steps, services), background prebuild, per architecture.
* Worker sessions in mount namespaces and cgroups, exclusive build slots
  (at most 24 h), services, port forwarding, secrets per run.
* Caches v2: `[cache.<name>]` with slot, machine (also per lockfile digest
  with remote install) and global scope (sccache, ccache, Go); pruning; the
  object store (`[storage]`, scoped worker grants); disk growth on aws-ec2.
* anyfs, one storage core ([caches-and-storage.md](caches-and-storage.md#anyfs-one-storage-core)):
  pages, layers and composable tier stacks (local -> volume -> store) with
  a virtual memory model (faults, writeback, ARC reclaim, thrashing);
  build slots are kernel overlays over committed layers (lazy restore and
  move by mounting), source projection and output overlay are anyfs
  configurations; access classes per path pattern drive placement,
  writeback delay, reclaim order and cross-slot dedupe; per-session read
  tracking and working-set readahead; every session is snapshotted and
  committed, the store head follows within 30 s of the commit; measured
  tier profiles and working sets in the scheduler and simulator (thrash
  time counted once, refetch learned per build key).
* Outputs: eager globs, `offshore fetch`, writable output overlay with
  cargo freshness; rust-analyzer `local` and `lsp-remote`.
* Tool adapters (cargo, go, node, generic); tests built remotely and run
  locally (`cargo test`, `cargo nextest run`, `go test`).
* Providers: local-docker, aws-ec2 (instance type fallback, arm64), oci
  (automatic compartment/VCN/bucket/OCIR/IAM, flex shapes), static hosts.
* Scheduler (bin packing, consolidation, warm pool windows, overcommit,
  history estimator), simulator and `sim export`.
* Scheduling v2: one cost model for every decision with
  `[scheduler] cost_optimization` (0 = fastest result, 1 = cheapest), disk
  as a resource, egress prices, slot handling (prune, resize, snapshot and
  restore), provider cooldowns, `sim -sweep`. See [scheduling.md](scheduling.md).
* Shared control plane: HTTPS with API key, fingerprint pinning or
  `--hostname` with Let's Encrypt, optional mutual TLS, container image,
  `deploy/aws/controlplane.yaml`; machine identities derived from one
  master key, tokens per lease; IAM template `deploy/aws/iam.yaml`.
* Operations: Prometheus metrics, JSON logs, OpenTelemetry tracing, event
  log, `report`, `top`, `why`, `status`, `doctor`, `init`, labels for
  agents and worktrees, prewarm and git hook.

## Verified (November 11, 2026)

Final integration run: `go test ./...` and the three e2e suites green
(embedded 163, remote core 97, remote full 169 checks); a ~120-crate
production workspace built through an embedded control plane on AWS (cold, no-op, incremental,
`cargo nextest run` split, output overlay with 0 local recompiles out of
634 units); machine loss on AWS with a lazy restore (13.7 GB head mounted
in 0.55 s, 4.6 GB paged in, warm build 7.5 s); an OCI machine smoke test.

## Open

* **Store head of large fresh slots**: written files are now hashed in
  the background during the session (fanotify close_write → anyfs.Hasher,
  identity cache dev/ino/size/mtime/ctime, ephemeral classes skipped, pages
  written back by class during the session), the commit reads the rest with
  parallel workers and streams early-class pages to the store while
  reading. Synthetic 4 GB fresh slot on a 125 MB/s-throttled disk: head
  78 s -> 1.8 s after the session, but the session itself took 228 s
  instead of 113 s (hashing, page files and writeback share the disk).
  Open: reflink page files (FICLONERANGE) instead of a second write,
  default hash rate limit per medium, live numbers for the production workspace on gp3.
* **Restore on slow root disks**: on gp3 roots (125 MB/s) the cost model
  prefers cold builds for big slots; learned cold times underestimate cold
  builds on fresh machines (empty compiler cache in the store). Planned:
  cold duration conditioned on compiler-cache state (global cache hit
  ratio per key/provider, fresh machine) and restore time from the
  measured aggregate page-in bandwidth per medium instead of nominal gp3;
  restored runs' warm samples include their page-in time.
* **Output overlay**: freshness needs the same cargo configuration locally
  and remotely (`[env]` of `$CARGO_HOME/config.toml` is not aligned, only
  rustflags); the first local run over a cold overlay fetches metadata;
  outputs the laptop never saw are lost if the machine dies before its
  commit; `[outputs] cache_size` applies per level (pages and files).
* **`offshore down`** leaves store objects until the bucket lifecycle
  expires them (one day).
* **Scheduler inputs**: per-session refault bytes are a machine-wide delta;
  the event-based refetch calibration is still per provider; thrash seconds
  are derived, not measured.
* **Storage details**: the userspace overlay rehashes files on chmod; torn
  non-empty clean pages are not detected; running machines get the cache
  medium policy only when recreated.
* **Users and quotas**: per-user API keys, quotas and cost limits on the
  control plane (today one global API key per pool).
* **Production domain**: run the shared control plane on a DNS name via
  `--hostname` (supported, see [control-plane.md](control-plane.md#from-no-domain-to-a-domain);
  current deployments pin a self-signed certificate).
* **GPU machines**: sizes and environments with GPUs (drivers in the
  machine image, device access in sessions).
* **OCI live coverage**: a build through the PAR store, Ampere A1 (arm64)
  and the ext4 root of shapes without NVMe were not run live.
* More providers (Scaleway, Kubernetes, ECS) and nextest across several
  machines remain open by choice.
