# Storage tiers under a Rust build: FUSE + overlayfs benchmark (spike)

Question: can a FUSE layer sit in the build path on a worker without hurting
Rust build speed, so `target/` can mix tiers (local disk, volume, object store)?
Measurement only; nothing here is production code. Feeds the design of the
unified `internal/anyfs` core (sourcefs + outputfs + worker tiers).

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
