# Storage tiers under a Rust build: FUSE + overlayfs benchmark (spike)

Question: can a FUSE layer sit in the build path on a worker without hurting
Rust build speed, so `target/` can mix tiers (local disk, volume, object store)?
Measurement only; nothing here is production code. Feeds the design of the
unified `internal/anyfs` core (sourcefs + output overlay + worker tiers).

Files: `tierfs/` (Go, go-fuse v2.12.1 prototype), `bench.sh` (runs inside the
container), `check.sh` (correctness checks), `run.sh` (host wrapper),
`results-2026-11-11.jsonl` (raw per-step results incl. FUSE op counts).

## Setup (2026-11-11)

- Host kernel 6.12.111 (FUSE passthrough since 6.9), ext4 on md RAID, 12 cores,
  shared and loaded (load 4..25 during runs). Docker `--privileged`, root.
- Toolchain: `rust:1-slim-bookworm` = rustc 1.99.1 for all layouts, sccache off,
  `CARGO_INCREMENTAL=1`, `-j 8`, `nice -n 10`.
- Workload: cargo-nextest @2cf55ec, `cargo build -p cargo-nextest` (348 units,
  ~590 crates in lock, debug target 3.2 GB / 5.8k files). The app workspace
  did not fit the ~15 GB free disk.
- Steps: clean; no-op; 3x (edit leaf `cargo-nextest/src/main.rs`, edit core
  `nextest-metadata/src/lib.rs`, 4 units); no-op. Restore layouts start from
  A's final target (same sources) instead of clean.
- tierfs: read-only lower serves a manifest (path -> type, mode, size, mtime_ns,
  sha256) with blobs from a CAS dir; read-write mode is go-fuse's loopback.
  Entry/attr timeouts 1 h, max_write 1 MiB, passthrough on open.

Layouts (CARGO_TARGET_DIR=/t):

| | layout |
|---|---|
| A | bind mount of a plain ext4 dir (baseline) |
| Cpt | /t = read-write tierfs (loopback), passthrough, no writeback cache |
| Cwb | same with writeback cache requested: kernel then refuses passthrough |
| Bclean | overlay(upper ext4, lower = empty RO tierfs), clean build |
| B | overlay(upper ext4, lower = RO tierfs over local CAS prefilled with A's target), default options |
| Bvol | B + `volatile` |
| Bmeta | B + `redirect_dir=on,metacopy=on` |
| D / Dpf | Bmeta, lower lazily fetched from a "store" dir, 5 ms/object + 200 MB/s shared pipe, 16 parallel; Dpf prefetches blobs <= 64 KiB at mount |

## Results

Wall seconds (median of repetitions; u+s = user+sys of cargo/rustc). The quiet
window (load ~5-10) is used for overhead %; A = A2/A3, Cpt = Cpt2/Cpt3.
Run-to-run noise on this host is about +-10 % for clean builds and +-0.5 s for
the incremental steps.

| layout | clean | restore (no-op on prefilled lower) | no-op | leaf edit | core edit |
|---|---|---|---|---|---|
| A ext4 | 62.6 (59.3..65.9) | - | 0.23 | 1.12 | 4.55 |
| Cpt rw FUSE passthrough | 62.1 (-1 %) | - | 0.42 (+0.19 s) | 1.42 (+27 %) | 5.32 (+17 %) |
| Cpt +noflush +noxattr | 63.5 (+1 %) | - | 0.42 | 1.37 (+22 %) | 5.25 (+15 %) |
| Cwb rw FUSE writeback, no passthrough (noisy window, vs A 82.9/1.82/7.56) | 92.5 (+11 %) | - | 0.47 | 2.82 (+55 %) | 8.68 (+15 %) |
| Bclean overlay, empty FUSE lower | 63.6 (+2 %) | - | 0.26 | - | - |
| B overlay default | - | 6.3 .. 8.8 (copies up 0.77 GB) | 0.29 | 1.9 .. 3.6, 1st 3.4 | 6.2 .. 8.0, 1st 21.5 / 22.7 |
| Bvol (+volatile) | - | 2.9 | 0.29 | 3.7 (noisy) | 10.8 (noisy), 1st 9.8 |
| Bmeta (+redirect_dir,metacopy) | - | 0.36 / 0.40 | 0.29 (+0.06 s) | 1.71 (+53 %), 1st 3.2 | 5.50 (+21 %), 1st 6.8 |
| D eager fetch on open | - | 7.97 (fetched 594 objs / 768 MB) | 0.29 | 3.3, 1st 14.8 | 9.7, 1st 15.5 |
| D lazy fetch on read | - | 3.43 (544 tiny objs, 2.9 s latency) | 0.33 | 3.4, 1st 18.7 | 8.7, 1st 14.2 |
| Dpf lazy + prefetch <= 64 KiB | - | 0.53 / 0.56 | 0.30 | 2.25 / 2.6, 1st 14.6 / 15.4 (1.73 GB fetched, 12.7 s waiting) | 6.9 / 7.4, 1st 10.6 / 12.7 |

FUSE ops reaching the daemon (passthrough on: 0 READ/WRITE bytes in all runs):

| | clean | no-op | leaf | core |
|---|---|---|---|---|
| Cpt | 185k (getxattr 80k, flush 19k, lookup 14k, open 10k) | 6.2k (open/flush/release 1149 each, statx 2.5k) | 8.9k | 26k |
| Cpt +noflush +noxattr | 86k | 5.0k | 6.7k | 16.6k |
| Cwb (no passthrough) | 264k + 72k READ = 6.6 GB through the daemon | 7.2k | 25k | 59k |
| Bmeta (lower only) | - | 3.7k | 6.5k | 16.5k |
| Bmeta +noflush +noxattr | - | 2.4k | 4.4k | 10.9k |

Daemon CPU (Cpt clean): 4.2 s user + 5.3 s sys for a 320 s CPU build (~3 %).
Cwb: 5.8 s user + 16.2 s sys. Pack of the 3.2 GB target into the CAS
(sha256, hardlinks): 2.5 s.

## Correctness (check.sh)

- Freshness: restore build on overlay(lower = tierfs) recompiles 0 units, the
  following no-op 0, with default options and with redirect_dir+metacopy. All
  5158 lower files keep their mtime_ns in the merged view; the 103 files cargo
  touches on a no-op (fingerprints, uplifted outputs) keep the lower mtime after
  copy-up except the 2 `.d` files cargo rewrites. FUSE rw: mtimes identical.
- Hardlink uplift (`debug/cargo-nextest` <- `deps/cargo_nextest-*`): works in
  all layouts (nlink 2). Default overlay reports different st_ino for the two
  names (no index/xino); with metacopy they match. Cargo does not care.
- Directory renames: renaming a lower-origin dir fails with EXDEV unless
  `redirect_dir=on` (host default is off). rustc's incremental session renames
  only touch dirs it just created in the upper, so builds were correct either
  way (no incremental warnings in any log), but enable redirect_dir anyway.
- Restore onto a fresh checkout (all *.rs mtimes = now): 5 workspace crates
  rebuild (6.2 s), dependencies stay fresh. Path-dep freshness is mtime based,
  so a restored target needs the source mtimes it was built with (sourcefs
  projects laptop mtimes, which is what we want).

## Findings / pitfalls

1. Passthrough is what makes FUSE free for data. With it no READ/WRITE reaches
   the daemon; without it (Cwb) 6.6 GB went through the daemon on a clean build.
2. Writeback cache and passthrough are mutually exclusive on 6.12: requesting
   CAP_WRITEBACK_CACHE makes the kernel refuse the backing fd registration and
   go-fuse silently disables passthrough for the whole mount. Do not request it.
3. Passthrough needs CAP_SYS_ADMIN in the init user namespace (privileged
   container) and backing files on a non-stacked fs (stack depth 0): the CAS /
   upper must be a bind-mounted host fs, not the container rootfs overlay.
   overlay-over-FUSE(passthrough) uses the maximum stack depth 2.
4. The remaining cost is metadata round trips: every open is a daemon round
   trip (open/flush/release), `statx` with btime is never cached, and the
   kernel asks `security.capability` on every write (80k getxattr on a clean
   build). ENOSYS for FLUSH and xattrs halves the op count. Opens cannot be
   avoided with passthrough (the backing fd is attached at open). This costs
   +0.2 s per no-op and +15..30 % on small incremental builds, ~0 on clean builds.
5. overlayfs default copy-up is the restore killer: cargo re-hardlinks uplifted
   artifacts on every build, so the first no-op copies 0.77 GB up (6-9 s, fsync
   per file; `volatile` 2.9 s) and the first incremental copies incremental
   session dirs (21 s). `metacopy=on` (needs redirect_dir=on) makes both
   metadata-only: restore 0.4 s, first core edit 6.8 s.
6. Lower fetch must be lazy on read, not on open: overlay copy-up opens the
   lower file to read its fileattr flags, so fetch-on-open pulled 768 MB on a
   no-op. Lazy handles are non-passthrough until the blob is local; the kernel
   rejects passthrough opens on an inode with cached-mode opens, so tierfs keeps
   new opens lazy while lazy handles exist.
7. Per-object latency dominates small files: cargo reads ~550 fingerprint and
   dep-info files serially on a no-op (2.9 s at 5 ms). Prefetching all blobs
   <= 64 KiB at mount (3305 blobs, 0.95 s in the background) fixes it.
8. The first build after an object-store restore is bandwidth bound: a leaf
   edit links against ~1.7 GB of rlibs (12.7 s at 200 MB/s). Prefetch rlibs of
   the likely-needed units (or restore with higher parallel bandwidth) before
   or while cargo starts; steady state afterwards is like Bmeta.
9. CAS packing with hardlinks (`pack -link`) shares inodes with the live target;
   cargo writes fingerprint files in place, which would corrupt CAS blobs.
   Production must copy or reflink into the CAS.

## Recommendation

- Worker, privileged: kernel overlayfs with `redirect_dir=on,metacopy=on`,
  upper on local disk, read-only FUSE lower with passthrough, entry/attr
  timeout long, no writeback cache, ENOSYS for flush/xattr/statx-btime, lazy
  fetch on first read, small-blob prefetch at mount. Viable: clean builds at
  parity (+2 %), restore of a 3.2 GB target in 0.4 s (local CAS) / 0.5 s (store,
  plus bandwidth-bound first link), steady incremental cost +0.6..1 s.
- Not recommended: target/ fully on a read-write FUSE (C). Data is fine with
  passthrough, but every metadata op of the build pays a round trip
  (+15..30 % incremental) and it buys nothing over overlay+local upper, which
  writes at native speed. Keep writes native; put only read-only tiers behind FUSE.
- For `internal/anyfs`: the read-only Layer (manifest + tiered blob fetch,
  lazy-on-read, prefetch) is the FUSE part; whiteouts, copy-up and merge
  should be kernel overlayfs where privileged. A userspace overlay would route
  every upper write and open through the daemon: per the Cwb/Cpt numbers that
  is acceptable for clean builds only with passthrough, and the metadata
  round-trip tax (+0.2 s no-op, +15..30 % incremental) is the floor. On the
  laptop (unprivileged, no passthrough, macOS) userspace overlay is fine for
  read-mostly output browsing / test archives, not as a build dir.

## Rerun on the real implementation (anyfs, Nov 11 2026)

Layouts E and F replace the prototype with `internal/anyfs` (`anyfsbench`:
pages and a base layer in a directory store, local page tier, materialized
files, read-only FUSE lower with passthrough, kernel overlay with
`redirect_dir=on,metacopy=on`). A, Bmeta and Dpf were rerun in the same
window for comparison (host load higher than in the first run, so the
overlay layouts are slower relative to A than above). Raw lines:
`results-anyfs-2026-11-11.jsonl`. Wall seconds:

| layout | clean | restore | no-op | leaf (3x) | core (3x) | commit |
|---|---|---|---|---|---|---|
| A ext4 | 59.6 | - | 0.23 | 1.22 1.25 1.21 | 5.14 5.38 4.99 | - |
| Bmeta prototype, local lower | - | 0.38 | 0.28 | 3.49 3.44 3.24 | 9.97 9.82 10.06 | - |
| E anyfs, warm slot | - | 0.40 | 0.29 | 4.45 4.77 3.66 | 12.63 13.23 9.75 | 3.28 |
| Dpf prototype, slow store + prefetch | - | 0.55 | 0.31 | 17.0 5.92 3.56 | 14.4 9.92 10.31 | - |
| F anyfs, slow store (5 ms, 200 MB/s) + prefetch <= 64 KiB | - | 0.45 | 0.29 | 19.94 5.86 3.64 | 17.68 11.64 9.95 | 3.61 |

- anyfs matches the prototype: restore 0.4 s (no-op on a 4.5 GB target),
  incremental steady state equal within noise; the first build after a
  cold restore is bandwidth bound (F leaf: 1.7 GB paged in, 11.4 s
  waiting), afterwards like warm.
- Pack of the 4.5 GB target into pages: 11.6 s (861 pages, sha-free
  BLAKE3 + copy into the store). Commit of the session (scan of 5160
  files, 3606 reused, 2.6 GB of changed files paged out to the store):
  3.3-3.6 s.
- F restore: 670 small files (8 MB) faulted by prefetch and the first
  cargo reads, 1.1 s waiting in the background, restore step 0.45 s.
- The overlay-over-FUSE incremental overhead vs. plain ext4 (+2-3x here,
  +50 %/+21 % in the quieter first run) is the metadata round trip cost
  of the FUSE lower under load; it is the same for prototype and anyfs.

## Incremental overhead of the slot overlay: profile and fix (Nov 11 2026)

Goal: incremental builds on a warm slot within +20 % of the plain cache
disk. Method: `run.sh alt ROUNDS A E M` (bench.sh `layout_alt`) keeps every
variant mounted for the whole run and bind-mounts one at a time at `/t`, so
all variants build the same source states back to back in one window
(order alternating per round); `anyfsbench` now reports the FUSE op
histogram of each lower (`LowerFS.Ops`: count and service time per op).
`XFS=14G` puts `runs/` on an xfs loop image with reflink, like the
workers' cache volume (`internal/supervisor/cache.go`); `-seed` adopts A's
files into the file cache by reflink (as a commit does), `-local-cap`
bounds the local page tier (disk). Variants:

- A: plain directory (A's target, continuing from S1)
- E: before: kernel overlay (`redirect_dir,metacopy`) over the read-only
  FUSE lower serving the tree by path (passthrough, 1 h entry/attr/negative
  timeouts)
- M: after (`anyfs.MountSlot`, now the worker's kernel slot mount):
  overlay `lowerdir=meta::files::lazy`, meta = local directory of sparse
  metacopy files with `trusted.overlay.redirect=/ab/<digest>`, files = the
  file cache (data-only), lazy = FUSE digest view (data-only, faults in
  what is not resident)

### Profile

FUSE ops reaching the daemon per build step on E (steady state; kernel
caches work: in a unit test 5 stats of a file and a missing name cost 4
lookups and 0 getattrs in total):

| step | lookup | open/release | readdirplus | service time in daemon |
|---|---|---|---|---|
| leaf | 179 | 2080 | 77 | ~30 ms |
| core | 2414 | 4035 | 380-460 | ~70 ms |

Lookups are new names (rustc's new incremental session dirs, new
artifacts: genuine negative lookups, not cache defeats); every open of a
lower file is a round trip (passthrough attaches the backing file at
open; no FOPEN_KEEP_CACHE/entry timeout avoids that). The daemon's own
service time is small; the cost is the round trip and wakeups under load.
M has 0 FUSE ops on a warm slot (the digest view is only consulted for
non-resident content).

The other half of the 2-3x on this host was not FUSE at all: with M on the
host's ext4, builds still slowed down round after round. `strace -T`
showed single `rename()` calls of 0.9-2.5 s on the overlay vs 0.06 ms on
A (`deps/cargo_nextest-<hash>.tmpXXX -> cargo_nextest-<hash>`, the rlib
archive rename). rustc deletes an output before writing the new one by
rename; in an overlay, deleting a name that exists in a lower layer leaves
a whiteout, so the rename replaces an existing inode, and ext4
(`auto_da_alloc`) then flushes the renamed file's data synchronously
(1.5-2 s for 200 MB in a microbenchmark, both for a whiteout and an empty
file as target). xfs does not do that (0.0 s), and workers' cache volumes
are xfs. Raw: `results-alt-ext4-2026-11-11.jsonl` (5 rounds, ext4).

### Results (xfs, 12 rounds, A/E/M alternating; `results-alt-xfs12-2026-11-11.jsonl`)

Wall seconds, median over rounds (min), and the median of the per-round
ratio to A (adjacent builds, robust against the host's load spikes):

| variant | restore | no-op | leaf | leaf / A | core | core / A |
|---|---|---|---|---|---|---|
| A plain dir (xfs) | 0.35 | 0.22 | 1.14 (1.00) | 1 | 4.33 (3.82) | 1 |
| E before: overlay over path FUSE lower | 0.56 | 0.27 | 2.07 (1.09) | 1.30 | 5.76 (4.42) | 1.29 |
| M after: meta layer + data-only files + lazy FUSE | 0.31 | 0.22 | 1.16 (1.05) | 1.04 | 4.54 (4.01) | 1.06 |

Earlier windows agree (`results-alt-xfs5/xfs10-*.jsonl`, per-round
ratio medians M/A leaf 0.76/1.02, core 1.01/1.27 with heavy load spikes;
minimum times M/A leaf +3 %, core +6 %). On ext4 (`results-alt-ext4-*`)
M/A was leaf 1.30, core 1.38 (E: 1.44, 2.11): the remaining gap there is
the rename flush above.

Commit was not rerun (disk: the host had 14 GB free; the commit path is
unchanged by this work). Writing the meta layer at mount takes 0.1 s for
the 5775 nodes of this target (parallel creates); restore = mount + no-op
build is faster than before (0.31 vs 0.56 s).

### Notes

- A file reclaimed from the cache while mounted falls through to the
  digest view on its next lookup (unit test `TestMountSlotMetaLayer`, also
  remount over a rebuilt meta layer: the upper's metacopy files keep their
  absolute digest redirect).
- Hosts whose cache directory is ext4 (local-docker on a dev box) pay the
  rename flush; mounting it `noauto_da_alloc` (or using xfs) avoids it.
- Ideas not pursued because M already removes FUSE from the warm path:
  longer timeouts (already 1 h, kernel honours them), eager copy-up of
  churn dirs (needs copies, defeats the 0.3 s restore; with metacopy the
  copy-up is metadata only anyway), promoting files into the upper.
