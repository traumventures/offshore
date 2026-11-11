# Scheduling

The scheduler (`internal/scheduler`) decides where a session runs (`Place`)
and how the pool changes (`Plan`) as pure functions of a pool snapshot
(interfaces `Policy` and `Estimator`). The orchestrator only gathers state
and executes the decisions, so policies can be tested and simulated without
machines. One orchestrator can span several providers; sizes of all
providers compete.

## Configuration

```toml
[scheduler]
policy              = "default"   # default (cost model) | binpack | classic
cost_optimization   = 0.5         # 1 = cheapest, 0 = fastest result
disk_price_per_gb_month = 0.08    # prices disk resizes
disk_budget    = "90%"            # cache disk machines fill before pruning ("150G": bytes)
source_cache   = "12.5%"          # fetched source contents kept, share of the disk budget ("20G": bytes)
estimator           = "history"   # history | static
cpu_overcommit      = 2.0         # only slows builds down
memory_overcommit   = 1.25        # requires soft_limit or swap, otherwise OOM
soft_limit          = 1.2         # memory.high = request * 1.2
swap                = true
max_cpu_pressure    = 0.0         # PSI some avg10; no new sessions above (0 = off)
max_memory_pressure = 0.4
limit_jobs          = true        # CARGO_BUILD_JOBS = granted cores
consolidate_below   = 0.5         # 0 = off
consolidate_target  = 0.75

[[scheduler.egress]]              # transfer prices for snapshot restores
from         = "aws"              # store: provider name, or "storage" for [storage]
to           = "oci"              # provider whose machines restore
price_per_gb = 0.09
bandwidth    = "50M"              # bytes/s

[[scheduler.warm_pool]]
days     = ["mon", "tue", "wed", "thu", "fri"]   # empty = every day
from     = "08:00"
to       = "19:00"                               # earlier than from spans midnight
machines = 1                                     # per recently used environment
timezone = "Europe/Berlin"                       # default: local time

[providers.aws]
type         = "aws-ec2"
max_machines = 4                                 # per provider, below pool.max_machines
```

| key | default | |
|---|---|---|
| `policy` | `"default"` | `default` (cost model); `binpack` and `classic` are earlier policies kept for comparison |
| `cost_optimization` | 0.5 | 0..1 trade-off of the cost model: 1 = cheapest, 0 = fastest result |
| `[[egress]]` | free within a provider, 0.09/GB at 50 MB/s across | `from` (provider or `storage`), `to` (provider), `price_per_gb`, `bandwidth` |
| `disk_price_per_gb_month` | 0.08 | price of extra disk, for resize decisions |
| `source_cache` | `"12.5%"` | source contents a worker fetched from the laptop are kept as a cache, reclaimed least recently used beyond this share of the disk budget or size (`"20G"`); open files never |
| `disk_budget` | `"90%"` | how much of a machine's cache file system build slots and machine caches may fill before the worker prunes: a percentage or a size (`"150G"`); sizes override it with their own `disk_budget`. The worker reports the budget in bytes and the scheduler plans prune/resize against it |
| `estimator` | `"history"` | `history` learns CPU and peak memory per repository/tool/subcommand from cgroup usage; `static` uses `[resources]` |
| `cpu_overcommit` | 2 | reserved CPU may exceed physical cores by this factor (>= 1) |
| `memory_overcommit` | 1 | > 1 needs `swap = true` or `soft_limit` |
| `soft_limit` | 0 (off) | sets `memory.high` to request x factor |
| `swap` | false | allow sessions to swap |
| `max_cpu_pressure`, `max_memory_pressure` | 0 (off) | PSI threshold 0..1 above which a machine takes no new sessions |
| `limit_jobs` | false | pass the reserved cores as build parallelism (`CARGO_BUILD_JOBS`, `OFFSHORE_JOBS`) |
| `consolidate_below` | 0.5 | drain the least loaded machine of an environment below this reserved share (0 = off) |
| `consolidate_target` | 0.75 | share the remaining machines may reach by taking its load |
| `[[warm_pool]]` | none | scheduled warm machines in addition to `pool.min_machines` |

Memory is never overcommitted beyond `memory_overcommit`; CPU is. Lifetimes
(`idle_stop`, `max_lifetime`), `min_machines`, `max_machines` and
`queue_timeout` are in `[pool]` ([configuration.md](configuration.md#pool-configuration)).

## Default policy: cost model

`[scheduler] policy = "default"` decides with a cost model
(`internal/scheduler/model.go`, `modelpolicy.go`). It scores every candidate
of a decision (run now on a machine, wait for a busy or starting machine,
start or create a machine, prune or grow a full disk, restore a slot
snapshot) by money and time to result and minimises

    r * cost / min_cost + (1 - r) * time / min_time      (r = cost_optimization)

normalised against the cheapest and the fastest candidate of the same
decision. `cost` is what the pool pays: the machine time a placement
occupies (full price where it extends a machine's uptime, its share where
the machine is paid anyway), started and created machines, the egress of a
snapshot restore and extra disk. `time` is queue wait + start or boot
latency + slot preparation (prune, resize, restore) + the run, using what
the estimator learned per key: warm and cold durations (cold per provider,
because global result caches live in a provider's store), restore times and
slot growth (`task_end.usage.slot_bytes_before/after`).
`cost_optimization = 1` optimises purely for cost (e.g. waits for a warm
slot instead of building cold on an idle machine), `0` purely for time,
`0.5` (default) accepts 10 % more cost for more than 10 % less time.
Rendezvous hashing breaks ties. Waits are bounded by `queue_timeout`.

**Tier stacks.** A size's caches live in a stack of tiers (`stack`, see
[configuration](configuration.md); `internal/anyfs`), and the model reads
the composite profile (`scheduler.Size.Stack`, a `medium.Stack`; anyfs.Profile is the same type):

- *lazy restore*: a restored or moved slot is mounted lazily, so a build
  waits only for its working set to fault in
  (`Profile.FaultTime(working set)`: per page latency of the backing tier
  plus the slowest transfer, at least the store bandwidth); the rest pages
  in in the background. A move uploads only the source's dirty pages when
  it reports a cache file system (`VM.Dirty`). The working set is learned
  per key from `task_end.usage.working_set_bytes` (80th percentile;
  unknown: the whole slot, as before).
- *thrashing*: when the working set exceeds the local capacity (the top
  tier: `VM.Capacity`, else the size's, else the disk budget), the build
  is charged `refetch x FaultTime(excess)` from the tier below the top.
  The refetch factor (refaulted bytes per byte of excess) is learned per
  build key from the sessions' `task_end.usage` (`refault_bytes` /
  (`working_set_bytes` - `local_capacity`), median of the thrashing runs,
  `Profile.Refetch`, from 2 runs on). Without that it is calibrated
  from the event log (`events.Refetch`, fed live by the orchestrator and
  replayed from the last 24 h at start): while a machine thrashes
  (`thrash` event, or a `machine_sample` whose VM thrashes), its refaults
  are `min(page_in, reclaim)` bytes; each episode (closed by the build's
  `task_end`, a sample below capacity, stop or destroy) divided by the
  excess of the working set over the capacity is one sample of a decaying
  average (weight 0.3) per provider and over all providers
  (`Pool.Refetch`; no data: 2). Thrashing is counted once: the worker
  reports the pages refaulted during a session (`refault_bytes`, machine
  wide: an upper bound with concurrent sessions) and the local capacity,
  the orchestrator turns them into `thrash_seconds` with the size's
  stack (`RefaultTime`, the same fault time the penalty uses), and the
  estimator learns warm/cold run times without them (base run times,
  `Profile.Thrash` keeps the median for reference). The model then adds
  the penalty of the candidate machine: on the machine the run thrashed
  on the prediction equals the observed run, on a machine whose local
  tier holds the working set the thrashing is gone (before, the learned
  run times already contained the thrash time and the penalty came on
  top). A machine that reports `VM.Thrashing` (or
  a working set above its capacity) gets this penalty for every placement.
  Against it the model weighs growing the disk (to 1.25 x the working set
  and twice that growth, priced until the machine expires; only when the
  disk is the local tier, not a volume below a fixed local disk) and, when
  scaling out, sizes whose local capacity fits the working set. With
  `cost_optimization = 1` a cheaper thrashing size can still win; at 0.5
  and below the bigger disk or size does.
- *restore and resize jointly*: every way to run (warm, restore, move,
  cold) is scored with each disk preparation (none, prune, grow to room
  for the slot / the working set / twice that), each with the thrash
  penalty of the local capacity it leaves, in one choice. A restore or
  move that needs room or whose working set does not fit grows the disk
  in the same step (`SlotAction.Grow`, slot action `resize+restore`)
  instead of restoring and thrashing or growing and running cold (before,
  a resize for a full disk replaced the restore and the build ran cold).
  A restore never prunes another repository's warm slot: in the sim that
  ping-pongs (team week with tiers: 3x thrashing, 5x restores).
- the medium slots persist on (the volume, else the top tier) prices
  storage, attach and resize cooldown.

Other inputs of the model: provider boot/start latencies (learned), snapshot
locations (reported by workers when they save slots), per-repository slot
sizes and the machine disk, and transfer prices between stores and
providers (`[[scheduler.egress]]`). Unlisted pairs are free within a
provider's own store and cost 0.09/GB at 50 MB/s across. A full slot is
handled by the cheaper/faster of: prune other slots (their next builds
become cold, which the model charges), grow the disk
(`provider.DiskResizer`, aws-ec2), or run elsewhere (cold, or restore a
snapshot). Capacity and quota errors take a provider out of scale-out for a
cooldown (2 min, doubling up to 30 min, reset on the next ready machine); a
failed start backs off for 2 minutes instead of retrying in a loop.

Per decision:

- **Place**: candidates are every way to run now (each accepting machine
  with free capacity: warm slot, restore of the best snapshot, move of a
  warm slot from an idle (e.g. draining) machine whose slot is newer than
  every snapshot (saved there, then restored here), cold; each after prune
  or resize if the disk is too full) and every way to wait
  (busy or starting machines of the environment, using the expected
  release times of their sessions and the learned spread of run times).
  Waiting is only considered within 80 % of `queue_timeout`.
- **Plan / scale out**: a timeline of the queued tasks (oldest 16) on the
  existing and starting machines is compared with the same timeline plus one
  start or create candidate (every allowed size, the three best stopped
  machines); the best option wins, repeatedly (up to 4 machines per plan).
  Waiting for sessions that end soon therefore beats booting a machine.
  Drained machines are undrained first.
- **Consolidate**: as below, on the forecast peak of the last 5 minutes,
  held for 2 minutes after a scale-up, never while anything is started.
  These flap guards are scaled by `cost_optimization`: full up to 0.5,
  fading out linearly to none at 1 (a drain followed by a create costs boot
  latency and a cold cache, keeping a lightly used machine costs money; at
  1 only money counts and the pool consolidates on the present
  reservation, like the binpack policy).
- **Idle stop**: an emptied consolidated machine is undrained when the
  others cannot take the forecast demand (while the flap guard is active).

The orchestrator feeds the model: `Task.Profile` (estimator), expected
releases (lease start + placement's expected run), provider latencies and
cooldowns, slot heads and layers (`layers.json` in the state dir), machine disk and
slot sizes (worker status), `scheduler.Forecast`. Slot actions of a
placement (prune via the worker slot RPC, `provider.DiskResizer`, restore)
run before the lease is returned and are logged as `slot_action` events;
capacity cooldowns as `provider_down`.

Consolidation is coordinated with creation: a machine is only drained when
the peak demand of the last 5 minutes (running plus queued) fits on the
others, never within 2 minutes of a scale-up, a drained machine is undrained
before anything is started or created, and an emptied consolidated machine
whose demand the others cannot take is undrained instead of stopped (regression trace `internal/scheduler/sim/testdata/prod-flap.json`, test
`TestRegressionDrainThenCreate`).

### cost_optimization sweep (Nov 2026)

`offshore sim -config internal/scheduler/sim/testdata/pool.toml -sweep
<trace>` (c7a sizes, static estimator, boot 60 s, start 20 s, queue_timeout
2m); binpack for reference:

| trace | binpack | r=0 | r=0.5 | r=0.6 | r=0.7 | r=0.8 | r=0.9 | r=1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| team-week cost $ | 60.03 | 66.33 | 65.68 | 64.26 | 63.92 | 61.72 | 60.02 | **58.12** |
| team-week turn p50 | 3m9s | 3m8s | 3m11s | 3m9s | 3m10s | 3m12s | 3m14s | 3m16s |
| team-week flaps | 171 | 19 | 20 | 21 | 24 | 37 | 42 | 140 |
| small-week cost $ | 17.92 | 20.56 | 20.42 | 20.08 | 20.02 | 19.26 | 18.78 | **17.38** |
| small-week turn p50 | 2m21s | 2m19s | 2m21s | 2m21s | 2m24s | 2m25s | 2m25s | 2m25s |
| small-week flaps | 35 | 12 | 11 | 12 | 12 | 10 | 9 | 16 |
| prod-flap cost $ | 1.27 | 1.76 | 1.66 | 1.62 | 1.52 | 1.39 | 1.40 | **1.26** |
| prod-flap turn p50 | 21s | 16s | 16s | 16s | 16s | 16s | 17s | 21s |
| prod-flap flaps | 7 | 1 | 0 | 0 | 0 | 0 | 2 | 6 |

Why the earlier model was more expensive than binpack and the knob barely
moved cost: swapping the model's parts for binpack's one at a time showed
that placement (machine cost share) and scale-out (sizing by the timeline)
are not the cause (the model's scale-out is cheaper than binpack's), but
consolidation is: the forecast peak, the hold after a scale-up, "never while
anything starts" and undraining emptied machines on the forecast kept
lightly used machines running, independent of `cost_optimization`, which
only weighed placement and scale-out choices. With the guards scaled by the
knob, `cost_optimization = 1` is cheaper than binpack on every trace and
0.5..1 is a real cost/time frontier (cost down, turnaround and flaps up).
Below 0.5 the trade buys almost nothing on these traces: turnaround is
dominated by run time, and keeping more idle capacity (tried: longer idle
stop at low values) raised cost by up to 30 % for a few seconds of p50.
`TestCheapestBeatsBinpack` asserts r=1 <= binpack on all three traces and 0
flaps on the production trace at 0.5; `TestRegressionDrainThenCreate` is
unchanged. The model needs a `queue_timeout` (default 2m) to bound waiting:
without one, cost-only decisions may leave tasks waiting indefinitely.

## Binpack policy (before the cost model)

`policy = "binpack"`, kept for comparison in the simulator:

- **Placement**: machines with a warm build slot for the repository come
  first; then machines that still have *physical* room (CPU overcommit slows
  tasks down, so it is the last resort); then best fit: the machine with the
  least normalised capacity left after placing the task. Rendezvous hashing
  of repository and machine breaks ties, so builds of a repository keep
  landing on the same machine when the fit is equal. Packing keeps other
  machines empty so that idle stop can release them.
- **Scale out**: queued tasks (largest first) are packed into the capacity
  that is there or on its way (free capacity of running machines, starting
  machines), then into consolidated machines that are undrained, then into
  stopped machines that are started. The rest is covered by new machines.
  Fragmented free capacity is not mistaken for room.
- **Sizing**: for the next machine the policy packs the remaining queue into
  every size and picks the lowest price per fraction of the queued demand
  the size absorbs (measured in the dominant resource). A single small task
  gets the cheapest fitting size; several queued tasks may justify one
  larger machine when it is cheaper per unit, and at equal cost per unit
  fewer machines win. Sizes that would need more machines than
  `max_machines` still allows lose against sizes that absorb more; a small
  stopped machine is not started when its slot is needed for a larger new
  machine, and an empty running machine is stopped to make room for a larger
  one when the pool is at its limit. Unknown prices (0) prefer the size
  absorbing the most demand, then the smaller one.
- **Architecture**: machines are only created from sizes of the
  environment's architecture (the repository's `resources.arch`; unknown
  environments are amd64), and a task no size of its architecture can hold
  is rejected.
- **Consolidation**: when the machines of an environment are reserved below
  `consolidate_below` and the others stay below `consolidate_target` after
  taking its share, the least loaded busy machine is drained (one at a time,
  never below the warm pool, never while tasks of the environment wait). New
  work lands elsewhere and the drained machine stops as soon as its sessions
  end. When demand returns it is undrained before anything is started or
  created.
- **Idle stop and warm pool**: idle machines stop after `idle_stop` unless
  the warm pool keeps them: `min_machines` per environment used in the last
  24 hours, raised by the scheduled `[[scheduler.warm_pool]]` windows.
- **Limits**: `max_machines` (pool and per provider) also bounds starting
  stopped machines.
- **Lifetime**: machines past `max_lifetime` drain and are destroyed with
  their volumes; idle machines stop and keep their disk.

`policy = "classic"` keeps the previous behaviour (rendezvous placement,
cheapest size for the aggregate demand, no consolidation) for comparison.

## Inputs: disk, slots and snapshots

The pool snapshot the policy sees contains, per machine, the disk
(`DiskTotal`, `DiskUsed`, the worker's `Budget`) and the size of each
repository's build slot (`Status.Disk`), and the snapshots held in object
stores (`Status.Heads`, plus those the orchestrator saved, each recorded
with the provider of the machine it came from). Sizes may declare `disk`. Sessions report their slot
size before and after the run (`task_end.usage.slot_bytes_before/after`).
The mechanisms for a full disk (prune, move by snapshot and restore, grow
the disk) are in [caches-and-storage.md](caches-and-storage.md).

## Simulator

`offshore sim` replays traces against the pool configuration:

```sh
offshore sim -config internal/scheduler/sim/testdata/pool.toml \
  internal/scheduler/sim/testdata/team-week.json internal/scheduler/sim/testdata/small-week.json
offshore sim -generate trace.json -profile team -seed 1 -days 5 -tz Europe/Berlin
```

`offshore sim -sweep trace.json` sweeps `cost_optimization` from 0 to 1
(steps of 0.1) and prints the cost/time frontier (Pareto points marked);
`-cost-optimization 0,0.5,1` compares chosen values, `-resize` lets machines
grow disks. Traces exported with `sim export` carry slot sizes, so disk,
slot growth, restores and multiple providers are simulated.

The simulation (`internal/scheduler/sim`) is discrete-event and drives the
policy like the orchestrator: every waiting task is placed in arrival
order, `Plan` runs while tasks wait and on a 15 s reconcile tick, expired
machines drain or are destroyed. It models what the policy cannot see:

- create (`-boot`) and start (`-start`) latency, per provider
  (`-boot aws=90s,local=5s`), 60 s minimum billing per start;
- CPU demand above physical cores slows every task on the machine
  proportionally; memory peaks above physical memory swap (`swap = true`,
  slowdown `(physical/used)^2`) or OOM-kill the newest sessions, which are
  retried up to three times;
- builds without a warm cache of the repository on their machine take
  `1 + cold-penalty` (default 1.5) times longer; caches survive stop/start;
- the estimator (`-estimator static|history`) learns from runs and OOMs;
- the cache file system of each size's tier stack: jobs with a
  `working_set` (bytes the build touches) restore lazily (the working set
  faults in at the stack's fault time, the rest in the background) and
  thrash when the working set exceeds the machine's local capacity (its
  disk; growing with resizes): `refetch x FaultTime(excess)` more build
  time, with the true factor `-refetch` (default 2). The policy sees this
  as `Machine.VM` (capacity, working set, thrashing) like the worker
  reports it and calibrates the factor from the simulated `thrash`,
  `reclaim`, `page_in` and `task_end` events like the orchestrator.
  Reports add `thrashed` builds, thrash time, page-in bytes and the
  calibrated factor; the sweep adds resizes, thrashed, refetch, bias and
  machines created per size. The simulated usage carries refaulted bytes,
  local capacity and thrash seconds like the orchestrator derives them,
  so the estimator learns base run times and per-key factors; `bias`
  (`Report.ThrashBias`) is the policy's predicted run (`Placement.Run`)
  over the actual run of the builds that thrashed or were expected to
  (above 1: thrashing overestimated). Jobs may carry their own true
  `refetch`; `-refetch-spread F` gives each repository `refetch x F^u`
  (u in [-1,1] from its name) to exercise the per-key factor. `-working-set` and `-slot-bytes` give jobs
  without cache data a working set and slot (tier stress on real arrival
  patterns), `sim/testdata/pool-tiers.toml` the c7a sizes with local disks
  (30G/30G/100G/200G) over the store.

Traces are JSON (`{"start": RFC3339, "end": seconds, "jobs": [{"at",
"repo", "env", "duration", "cpu", "memory", "peak_memory", optional
"slot_bytes", "slot_growth", "working_set"}]}`) or CSV with
the same columns (`at` in seconds or RFC 3339, optional `# start=` line).
The generator produces bursty workday load (morning ramp, lunch dip,
afternoon peak, follow-up bursts, night trickle) for several repositories
sharing environments; `sim/testdata` contains a team week (6 repositories,
3 environments, 3405 builds), a small week (3 repositories, 1019 builds) and
a CSV sample.

The report shows cost, machine-hours, machines created, p50/p95/max wait,
p50/p95 turnaround (wait + run), CPU/memory utilisation of billed capacity,
slowdown (run time / warm full-speed duration), OOM kills and warm cache hit
rate.

## Results

Sample traces, AWS c7a sizes (large/xlarge/2xlarge/4xlarge at on-demand
prices), `max_machines = 16`, `idle_stop = 20m`, `cpu_overcommit = 2`,
boot 60 s, start 20 s, static estimator unless noted:

| scenario | trace | policy | cost $ | machine-h | wait p95 | wait max | turnaround p95 | OOMs |
|---|---|---|---:|---:|---:|---:|---:|---:|
| base | team | default | **60.03** | 483.7 | **20s** | 7m33s | 13m36s | 0 |
| base | team | classic | 75.17 | 556.0 | 1m0s | 23m51s | 13m26s | 4 |
| base | small | default | **17.92** | 174.5 | 20s | 1m0s | 9m42s | 0 |
| base | small | classic | 22.76 | 221.6 | 20s | 1m0s | 9m38s | 0 |
| history estimator | team | default | **60.00** | 348.0 | **20s** | 1m8s | **10m12s** | 0 |
| history estimator | team | classic | 83.20 | 471.0 | 2m35s | 2h2m | 13m27s | 0 |
| history estimator | small | default | **21.08** | 148.2 | **20s** | 1m0s | **6m38s** | 0 |
| history estimator | small | classic | 25.88 | 192.9 | 3m46s | 1h8m | 9m29s | 0 |
| max_machines = 8 | team | default | **62.63** | 318.0 | **1m16s** | 17m39s | **13m6s** | 4 |
| max_machines = 8 | team | classic | 92.21 | 373.8 | 8m32s | 2h3m | 16m38s | 4 |
| cpu_overcommit = 1 | team | default | **71.34** | 388.3 | 20s | 1m0s | 7m9s | 0 |
| cpu_overcommit = 1 | team | classic | 93.39 | 523.1 | 20s | 10m55s | 7m0s | 0 |
| no consolidation | team | default | 74.26 | 616.0 | 20s | 7m51s | 13m42s | 4 |
| warm pool 08-19 | team | default | 71.90 | 509.4 | 20s | 2m21s | 13m27s | 4 |
| warm pool 08-19 | team | classic | 89.66 | 570.8 | 37s | 14m20s | 13m2s | 8 |
| boot 120s, start 40s | team | default | **63.40** | 463.1 | **40s** | 3m39s | 13m24s | 4 |
| boot 120s, start 40s | team | classic | 75.20 | 584.2 | 1m2s | 22m39s | 13m22s | 4 |

The default policy is 16-32 % cheaper in every scenario at equal or better
p95 wait (p50 wait is 0 for both). Consolidation accounts for most of the
saving at low load (team: 74.26 → 60.03), packing into fragmented capacity
and limit-aware sizing remove the long waits of the classic policy. The
price of tighter packing with `cpu_overcommit = 2` is a slightly higher
slowdown (1.54 vs 1.48) and turnaround (+1 %); with `cpu_overcommit = 1`
turnaround is equal. The warm pool costs about $12 a week for three
environments and here only lowers the maximum wait, because start latency
(20 s) is already short. `TestSampleTraces` in `internal/scheduler/sim`
asserts the cost/p95 claim on the sample traces.

### Refetch calibration and joint resize+restore (Nov 2026)

`offshore sim -sweep` on `sim/testdata` (`prod-flap.json` is from the
production event log, small/team week synthetic), before and after:

- `pool.toml`: identical frontiers on all three traces (the traces carry
  no working sets or slot sizes, so neither the refetch factor nor the
  joint restore+resize applies; the new `refetch` column is `-`).
- `pool-tiers.toml -resize -estimator history`, every job with a 40G
  working set and 25G slots (`-working-set 40G -slot-bytes 25G`): the
  frontier is roughly unchanged at r <= 0.6 and better at high r with
  fewer thrashing builds (team week r=0.8: $79.50 / turnaround p95
  13m21s / 288 thrashed before, $78.52 / 12m46s / 333 after; r=0.9:
  $78.66 / 14m32s / 753 → $76.54 / 13m43s / 311; r=1: $94.69 / 20m39s /
  1600 → $89.55 / 20m4s / 1194; small week r=1: 783 → 457 thrashed,
  p95 17m5s → 15m53s, +$1.27). Letting restores prune other warm slots
  instead made team week r=0.7 $89.70 / 17m43s (3x thrashed), hence no
  prune for restores.
- true refetch 6 (`-refetch 6`): calibrated (6.0) versus the old fixed
  factor 2: team week r=1 $83.66 / 18m3s / 133 thrashed versus $115.68 /
  33m20s / 966; r=0.5 $84.32 / 10m39s versus $86.00 / 13m55s; small week
  r=1 $34.06 / 14m43s versus $41.36 / 31m28s.

### Thrash time counted once, refetch per key (Nov 2026)

Before, learned run times contained the thrash time of the runs and the
model added the thrash penalty again; the refetch factor was one per
provider. Now runs are learned without their thrash time and the factor
per key (see *thrashing* above). `offshore sim -sweep -policy default
-config pool-tiers.toml -resize -estimator history -working-set 40G
-slot-bytes 25G` on `team-week.json` / `small-week.json`, before → after
(cost $ / turnaround p95 / thrashed / bias):

- team week r=0.7: $80.79 / 12m12s / 113 / 1.11 → $80.30 / 12m10s / 55 /
  0.72; r=0.8: $81.59 / 13m32s / 414 / 1.30 → $77.15 / 12m19s / 50 /
  0.74; r=0.9: $78.23 / 14m25s / 393 / 1.23 → $78.54 / 13m46s / 164 /
  0.79; r=1: $84.74 / 17m29s / 766 → $87.79 / 18m49s / 675; r <= 0.6
  within ±$1 and ±30s.
- small week r=0.8: $34.10 / 10m32s / 149 / 1.33 → $31.98 / 11m51s / 142
  / 0.91; r=0.9: $33.76 / 15m17s / 435 / 1.24 → $30.87 / 10m24s / 111 /
  0.96; r=1: $30.50 / 12m34s / 289 → $32.15 / 15m17s / 406; r=0.4–0.6
  $41.14/38.96/36.57 → $40.42/39.12/37.02.
- with per-repository factors (`-refetch 4 -refetch-spread 4`, factors
  1–16): team week r=0.7 $83.11 / 11m55s / 20 → $82.01 / 11m1s / 7, r=0.9
  $79.38 / 11m47s → $80.30 / 11m28s, r=1 $79.86 / 12m52s / 32 → $80.65 /
  12m46s / 45; small week r=0.8 $40.47 / 9m19s → $38.90 / 9m12s, r=0.9
  $39.48 / 10m8s → $37.00 / 9m32s, r=1 $38.40 / 12m0s → $32.54 / 10m0s.

The bias of builds the policy expected to thrash is 1.02 cold and 0.93
warm (small week r=1; restores 0.74, contention and restore time are not
part of it); the rest of the bias below 1 in the table comes from builds
whose working set was not known yet (first runs of a key, predicted
without thrashing), restores and CPU contention. At r=1 the
policy now sees thrashing at its real (lower) cost and accepts more of it
on cheap sizes; elsewhere fewer builds thrash because predictions on
fitting machines no longer carry the old thrash time.

## Calibrating the simulator from real runs

The orchestrator (control plane, or the daemon in embedded mode) always
writes an analysis event log to its state directory:
`events-YYYY-MM-DD.jsonl` (UTC days, one JSON object per line, kept
`[telemetry] events_retention_days` days, default 30; files are capped at
256 MiB per day). Task events record each request (`task_placed`: arrival,
estimate, placement and reason, queue wait, machine, size, price) and each
finished session (`task_end`: reservation, measured peak memory, CPU
seconds, wall time, OOM, warm build cache, sccache counters). Machine events
record the pool (`machine_create`, `machine_ready`, `machine_start`,
`machine_stop`, `machine_destroy`, `machine_drain`, `machine_undrain` and
`machine_state` for changes observed from the provider, e.g. a machine that
stopped itself when idle) with provider, size, price, capacity
and provider latency. In remote mode the daemon already reports usage on
release, so the control plane's log covers every client.

`offshore report` summarises a window from it:

```sh
offshore report --since 7d          # or --since 2026-11-01 --until 2026-11-08, --json
```

- cost: machine-hours × price per UTC day and per provider/size; attributed
  to repositories by the sessions' reserved share of their machine (mean of
  CPU and memory share), the rest is idle cost;
- utilisation: reserved CPU/memory vs. capacity of the machines that were
  up, CPU time and peak memory vs. reservation (overprovisioning);
- queue wait p50/p95/max of assigned requests, requests by outcome;
- warm vs. cold build caches, summed sccache counters;
- estimator accuracy: measured peak / memory estimate (p50, p95, mean,
  MAPE), sessions above their estimate, OOM kills, busy cores / CPU estimate.

`offshore sim export` turns the finished sessions into a trace the
simulator replays:

```sh
offshore sim export --since 7d -o real-week.json
offshore sim -config pool-candidate.toml -policy default,classic real-week.json
offshore sim -config pool-candidate.toml -estimator history real-week.json
```

Each job keeps its repository id, environment hash, arrival time (relative
to the hour of the first arrival), requested memory (the estimate the
orchestrator packed with) and measured peak memory. The duration is
normalised to what the simulator models, a warm build at full CPU: `cpu` is
the measured number of busy cores (CPU seconds / wall time), so that
`duration × cpu` reproduces the measured CPU time, and cold runs are divided
by `1 + cold-penalty` (export flag `-cold-penalty`, default 0.5 like the
simulator's, which adds the penalty back for cold machines). `-request-cpu`
keeps the requested CPU instead. Workflow: export a representative week,
check that `offshore sim` with the current pool configuration
reproduces the report's cost and queue wait roughly (adjust `-boot`,
`-start` and `-cold-penalty` until it does), then compare candidate sizes,
overcommit factors and policies on the same trace.

## Open issues

- The orchestrator drops environments from its recent list after 24 hours,
  so a scheduled warm pool only applies to environments used in the last
  day (on Monday morning after a quiet weekend no machine is pre-started).
- Consolidation causes stop/start churn (hundreds of stops per week in the
  team trace); EC2 bills a minimum of 60 s per start, which the simulator
  includes, but EBS/AMI warm-up effects after a start are not modelled.
- The simulator assumes peak memory for the whole run and constant CPU use;
  real builds have phases.
- `sim export` cannot tell a build that was slowed by CPU contention from
  one that is not parallel: both appear as fewer busy cores for longer. The
  event log has no per-session pressure yet.
