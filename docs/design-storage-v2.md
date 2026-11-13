# Storage v2: one notion of space

Status: implemented (worker `internal/worker/ledger.go`, `prune.go`,
`slottrim.go`; scheduler `internal/scheduler/ledger.go`, `model.go`;
sim `internal/scheduler/sim/disk.go`). Companion of
[design-scheduler-v2.md](design-scheduler-v2.md); details of the tiers in
[caches-and-storage.md](caches-and-storage.md).

## Goals, in order

1. **Warm starts everywhere.** Every build starts warm on whatever machine
   it lands on: its own slot, a sibling seed, or a lazy restore of the
   store head (counted warm when its run is within 15 % of the warm run).
2. **Caches never pin work.** A warm slot is an advantage, never a
   constraint: the store head is the source of truth, every local byte is
   a demotable copy of it, so any machine can take any repository.
3. **Space is invisible.** No ENOSPC, no build fails or waits because of
   disk. Space is made ahead of need, in the background, by demotion to
   the store (which keeps warmth) rather than deletion.

Then cost, then p95. The simulation gate checks exactly this order.

## What went wrong (prod, Nov 11-12)

* Two meanings of bytes. The scheduler subtracted *logical* slot sizes
  (overlay merged views: 367 GB of "slots" on a disk using 165 GB) and asked
  workers to "prune N bytes"; the worker pruned merged-view files (lower
  files become whiteouts: nothing freed until the page tier drops them) and
  reported logical bytes. 93 prunes on Nov 11, 26 failed ("no build slot"),
  the rest re-requested the same 33 GB every 10 minutes for hours.
* Slots grow without bound: a warm Rust build adds 0.64 GB (p50), 5.5 GB
  (p90); stale units are only trimmed above the *machine* budget, so three
  machines reached 100 % (196/196 GB) and sessions hit ENOSPC.
* Heads reach the store late (task end to flushed head: p50 45 s, p90
  6.7 min, p95 16 min); a slot dropped before its flush lost warmth.

## The ledger: one notion of space

The worker owns the physical truth and reports it as a ledger
(`proto.DiskLedger` in `Status.Disk`), measured with statfs and the cache
file system's own counters, never with logical tree sizes:

    total = used + free
    used  = pinned + Σ reclaimable[class]          (other = what nobody claims, pinned)
    reserve = floor + Σ_running forecast growth     (headroom that must stay free)
    room  = free + Σ reclaimable − reserve          (what a new session may take)

* **pinned**: leased slot uppers, dirty data not yet in the store, the
  rest of the file system (images, system, unaccounted).
* **reclaimable**, one entry per class (and repository) with physical
  bytes and a **cost**: build seconds expected to be lost.
  `cost = rebuild_seconds × P(reuse)`, `P(reuse) = gap / (gap + idle)` with
  `gap` the slot's mean time between runs (1 h without history).

| class | what | rebuild | demotes to |
|---|---|---|---|
| garbage | stale build units in a slot (Rust: units unread for 3 runs; other: files unread for 5 runs) | ≈ 0 | nothing (rebuilt only after a branch switch) |
| resident | clean pages and materialized files | refetch at store bandwidth | store |
| writeback, dormant | written-back blobs, local tiers of repositories without slots | refetch | store |
| source | faulted-in source contents | refetch from the laptop | laptop |
| slot | an idle slot whose state is flushed (head = slot) | lazy restore of its working set | store head |

An idle slot that is *not* flushed is pinned until its commit lands; the
reclaimer flushes it first (demotion never loses warmth). `floor` is
max(5 GB, 3 % of the disk, the disk beyond `disk_budget`); the growth
forecast per running session is twice the learned median slot growth of
its usage key (learning store), passed with the lease (`Exec.SlotGrowth`,
2 GB unknown).

## One reclaim order, driven by watermarks

One function (`Worker.reclaim(free)`) frees physical bytes until
`free ≥ want`, taking reclaimable entries in ascending cost per byte, and
reports what statfs says it freed. It runs:

* in the background whenever `free < 2 × reserve` (high watermark), down to
  nothing it does not need: it stops at `free ≥ 3 × reserve`;
* synchronously on the 1 s guard tick while sessions run and
  `free < reserve` (the ENOSPC guard: it may then also take unflushed idle
  slots after an urgent flush, and finally admits nothing new);
* on the scheduler's request before a placement (`SlotOp reclaim`, the
  free bytes the session needs), bounded by 20 s.

**Per-slot budget, trimmed in the slot.** Every slot has a budget:
the sum of its caches' `max_size`, else `2 ×` its size after its first
(cold) build, at least 10 GB and at most 40 % of the disk. After every
session the background trim removes garbage (the existing prune stages 1
and 2: old incremental sessions, then units unread for 3 runs, stalest
first) when the slot exceeds 75 % of its budget, and size-trims to the
budget above it. Trimming happens in the upper and is committed, so the
store head shrinks too; pages no longer reachable from any mounted chain
or flushed head become reclaimable resident data. This keeps a long-lived
Rust slot at a steady state instead of growing 1.8 GB per build. It is
generic: `prune = "rust"` knows cargo units, `prune = "lru"` (node_modules,
Go caches, anything) trims files by last read run, `none` only the budget
of the machine.

## Disks: size, grow, never shrink, replace

* **Initial size** per size (`disk`, aws `disk_gb`), default 200 GB for
  8+ cores. gp3 at 6000 IOPS/500 MiB/s costs 0.08 $/GB-month + 30 $/month
  for the provisioned extra; the scheduler prices a stopped machine's disk
  with both (`medium` profile `price_per_gb_month` includes them pro rata).
* **Grow early, in the background**: a Plan action (`Grow`, not part of a
  placement) grows a disk by max(50 %, 50 GB) when `pinned + reserve`
  exceeds 60 % of it, because EBS allows one change per 6 h (the
  orchestrator records the resize for the cooldown). In the sim 60 %
  beat 70 % (more warm starts, no disk waits) and 50 % (cost).
* **Never shrink** (EBS cannot): a machine past `max_lifetime` is replaced
  with the configured size; warmth moves through the store.
* Local NVMe (`*d` types) fits this model: losing the disk on stop costs a
  lazy restore, not warmth, once heads are flushed.

## Store retention

Objects under `cache/` live 24 h (the cleanup guarantee in
[operations.md](operations.md#24-h-guarantees-and-cleanup)); active chains
are rewritten after 12 h. A repository's head therefore survives about a
day without use. Cost: 30 GB of head = 0.023 $/GB-month × 30 GB = 0.023 $/day
in S3, versus a cold Rust build of ~10 min on an x8 (0.10 $ plus the wait at
time_value). Keeping heads 7 days would pay off for every repository built
at least weekly, but it breaks the 24 h guarantee, so it is a pool choice:
`[storage] retention` (default `"24h"`, at most `"7d"`) sets the store TTL
(carried in the store grant), the bucket lifecycle (aws stack parameter
`CacheRetentionDays`, oci rule), the layer TTL of the orchestrator's GC
and the chain rewrite age (half of it). Unreachable layers are deleted
every 10 min as today.

**Heads reach the store sooner.** The commit waited for the 30 s head
deadline, queued behind other repositories' flushes (one maintenance
lock) and skipped captures while a commit ran. Now maintenance runs per
repository, a skipped capture is queued and committed right after, pages
upload outside the commit lock and the head moves as soon as its pages
are clean. What remains is the commit itself: background hashing was
capped at 32 MB/s on two files, so a cold build's outputs were mostly
left to the commit. Now the hasher runs on idle cores at the lowest CPU
and IO priority with a quarter of the cores, and the 32 MB/s cap applies
only while the machine's IO is under pressure (PSI `some avg10` ≥ 10 %);
the commit reads with half the cores and hashes a large file's page keys
and digest on separate cores (2.2 GB/s per file instead of 0.7). Build host
bench (2.25 GB of cargo target dirs written at 150 MB/s): session end to
committed tree 1.7–2.2 s before, 50–75 ms after (hasher kept up), 0.8–1.4 s
under IO pressure. Digests are unchanged (BLAKE3): existing heads stay
valid.

## How the scheduler consumes it

`scheduler.Machine` carries `Disk` (the ledger) instead of `DiskUsed`,
`DiskBudget` and logical `SlotBytes` (kept only as the warmth map).
For a placement of task `t` on `m`:

    need    = t's slot growth (warm) or its slot size (restore, cold)
    missing = need − room(m)

* `missing ≤ 0`: no disk handling; the run is as warm as it is.
* reclaimable covers it: `SlotAction.Reclaim = need`; time 2 s plus Σ
  cost of the entries it takes (lost warmth of others), usually ≈ 0 since
  slots demote to the store.
* else grow (`SlotAction.Grow`) or the option does not exist (the
  disk-tight penalty and the logical prune are gone); a new machine is
  costed with the disk it will report (`FreshDisk`), and only when no
  machine or size can make room is a fresh one taken anyway (1 h penalty).
  A grow during a placement counts its 60 s twice, so other capacity of
  the same latency wins: sessions should not wait for disks.

Two pricing errors the ledger made visible are removed: a restore no
longer pays for "a second warm copy" (local copies are a demotable tier of
a fixed-size disk; only a grow costs money), and a learned cold run is at
least 1.25× the warm run (a few short cold samples made cold builds look
as fast as warm ones; in the growth sim two thirds of the cold builds had
a fresh head to restore).

Restores are costed with the working set (`LazyRestore`, page_in
calibrated); the event log stays observability only, the forecast inputs
come from the learning store.

## Gate (`sim/storage_gate_test.go`)

Physical disk model calibrated on Nov 11-12 (growth p50 0.64 GB / p90
5.5 GB per warm build, working set 0.21 of the slot, 17.5 GB system, head
lag 10 s + 15 MB/s before, 2 s + 32 MB/s after). Base = scheduler and
worker before storage v2 (`testdata/storage-gate-base.json`), 8 cases × r
∈ {0.2, 0.8}; full tables in `testdata/storage-gate-{v1,v2}.md`.

| sum over 16 runs | warm starts | ENOSPC | disk waits | failed | cost |
|---|---|---|---|---|---|
| base | 90.2 % | 246 (disk/tiers 244, nov11 on 100 GB disks 2) | 383 (19 h) | 109 | $441 |
| v2 | 94.7 %, ≥ base in 16/16 | 0 | 1 (a 60 s grow on disk/tiers' 30 GB disks) | 0 | $408 |

Turnaround p95 ≤ base in every run (disk/tiers r=0.8: base completed
nothing). Cost ≤ base in 15/16; team-week r=0.2 costs +0.9 % for a 17 %
lower p95 (objective $543 vs $593).

Not modelled: the trim's rebuilds after branch switches, NVMe sizes,
retention beyond 24 h.
