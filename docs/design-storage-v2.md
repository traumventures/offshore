# Storage v2: one notion of space

Status: design, implemented in phases (see the end). Companion of
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
max(5 GB, 3 % of the disk); the growth forecast per running session is
the p90 slot growth of its usage key from the learning store, passed with
the session (`Exec.SlotGrowth`).

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
* **Grow early, in the background**: a lifecycle action (not part of a
  placement) grows a disk by max(50 %, 50 GB) when `pinned + reserve`
  exceeds 70 % of it, because EBS allows one change per 6 h.
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
`[storage] retention` (default `"24h"`, at most `"7d"`) sets the store TTL,
the bucket lifecycle (`ExpirationInDays`), the layer TTL of the
orchestrator's GC and the compaction age (half of it). Unreachable layers
are deleted every 10 min as today.

## How the scheduler consumes it

`scheduler.Machine` carries `Disk` (the ledger) instead of `DiskUsed`,
`DiskBudget` and logical `SlotBytes` (kept only as the warmth map).
For a placement of task `t` on `m`:

    need    = t's slot growth (warm) or its slot size (restore, cold)
    missing = need − room(m)

* `missing ≤ 0`: no disk handling; the run is as warm as it is.
* reclaimable covers it: option `SlotReclaim{Bytes: need}`; time 2 s +
  the flushes it waits for; its time cost is Σ cost of the classes it takes
  (lost warmth of others) — usually ≈ 0 since slots demote to the store.
* else grow (`SlotResize`, as today) or the option does not exist; the
  disk-tight penalty is gone: a machine that cannot make room is not a
  candidate, so no session ever runs into a full disk.

Restores are costed with the working set (`LazyRestore`, page_in
calibrated); the event log stays observability only, the forecast inputs
come from the learning store.

## Phases

1. Sim: physical disk model (ledger, growth during runs, garbage, trim,
   reclaim, ENOSPC, store lag, grow) with the v1 and v2 worker behaviour;
   gate on warm-start rate, ENOSPC, disk waits, cost, p95.
2. Worker: ledger, reclaim(free), watermarks, ENOSPC guard, slot budget
   and trim, `SlotOp reclaim` (replaces `prune`).
3. Scheduler and orchestrator: ledger in `Machine`, `SlotReclaim`,
   background grow, disk price incl. provisioned IOPS/throughput.
4. Store retention setting.
