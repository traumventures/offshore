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
policy              = "default"   # default (option policy) | costmodel | binpack | classic
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

[providers.oci]
type    = "oci"
enabled = false                                  # no machines here (default true)
```

| key | default | |
|---|---|---|
| `policy` | `"default"` | `default` (option policy with the projected-start gate and stop decay of round 2); `option` (the option policy of round 1), `costmodel`, `binpack` and `classic` are earlier policies kept for comparison |
| `cost_optimization` | 0.5 | 0..1, the single trade-off knob: an hour of turnaround is worth (1-r)/r hours of the cheapest size; 1 = cheapest, 0 = fastest result |
| `[[egress]]` | free within a provider, 0.09/GB at 50 MB/s across | `from` (provider or `storage`), `to` (provider), `price_per_gb`, `bandwidth` |
| `disk_price_per_gb_month` | 0.08 | price of extra disk, for resize decisions |
| `source_cache` | `"12.5%"` | source contents a worker fetched from the laptop are kept as a cache, reclaimed least recently used beyond this share of the disk budget or size (`"20G"`); open files never |
| `disk_budget` | `"90%"` | how much of a machine's cache file system build slots and machine caches may fill before the worker prunes: a percentage or a size (`"150G"`); sizes override it with their own `disk_budget`. The worker reports the budget in bytes and the scheduler plans prune/resize against it |
| `estimator` | `"history"` | `history` learns CPU (average busy cores, capped by the configured value) and memory (p90 of the peak plus 25 %; before 3 runs the configured value raised to the largest observed peak plus 25 %) per repository/tool/subcommand from cgroup usage; `static` uses `[resources]` |
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
(`idle_stop`, `max_lifetime`), `min_machines`, `max_spend_per_hour` and
`queue_timeout` are in `[pool]` ([configuration.md](configuration.md#pool-configuration)).

## Scale-out bounds

There is no configured machine count (`max_machines` is gone, also per
provider). Three things bound the pool instead (`internal/scheduler/limits.go`):

- **The policy**: the default policy starts or creates a machine only for
  a queued request whose projected start is later than the machine would
  be ready. The projection list-schedules the queue (SRPT with aging,
  owners round robin) on the running and starting machines using the
  sessions' pessimistic release ETAs; a reservation without a lease
  estimate is not counted on to end. "Later" means the request's total
  wait would exceed the start/create latency plus the hedge margin
  (30s/(1-r)), at cost_optimization 1 half the queue timeout. Rejected
  candidates carry `no queued request would wait longer than the machine
  takes` in their decision.
- **Runaway guard** against bugs and feedback loops: at most
  ⌈R + β√R⌉ running and starting machines plus one per environment with
  machines or queued requests plus the warm pool. R is the offered load in
  machines: per environment the larger of the 15-minute time average of the
  reservation (running plus queued, `Demand.Load`: arrival rate × resource
  seconds by Little's law) and the present one, divided by the mean
  admissible capacity of its sizes, in the dominant resource. β is
  2 - 1.5·r (2 when time counts, 0.5 when only money does). A request
  that waited longer than the fastest create plus the hedge margin (at
  r = 1: 80 % of the queue timeout) lifts the guard for its environment;
  such actions carry `capacity exception (guard N): …` in their reason and
  the orchestrator logs them as `capacity exception`. Decisions blocked by
  the guard carry `runaway guard reached`.
- **Hard limits**, never lifted: providers with `enabled = false`, the
  provider accounts' limits (`Pool.ProviderLimit`) and the optional
  `[pool] max_spend_per_hour` (hourly price of running and starting
  machines). Account limits are discovered: aws-ec2 reads the On-Demand
  standard vCPU quota (Service Quotas `L-1216C47A`) every 10 minutes and
  subtracts the account's other running instances; any provider that
  answers a create with a quota/limit error is capped at the vCPUs the pool
  runs there for an hour (not disabled, no cooldown; without a machine
  there it cools down as before). Account-type limits (free tier) and auth
  errors still disable the provider. Hard limits are enforced after every
  plan step (`enforceLimits`), whichever step proposed the machine.

## Default policy: option policy

`[scheduler] policy = "default"` is the option policy below plus the
projected-start gate of *Scale-out bounds* (round 2); `policy = "option"`
is the option policy without it (round 1).

`internal/scheduler/options.go`, on the
candidate machinery of `model.go` and `modelpolicy.go`) enumerates every
way to serve a request explicitly: run now on a machine (warm slot,
restore, move, cold; each after prune or resize), wait for a busy or
starting machine (pessimistic release ETA, see below), start a stopped
machine, create a size on a provider. Each option has an expected time to
result and an expected cost, and the option policy minimises

    cost + timeValue * time,   timeValue = price of the cheapest size * (1 - r) / r

(r = `cost_optimization`): at 0.5 an hour of turnaround weighs as much as
an hour of the cheapest machine, at 0.2 (production) four times as much,
at 0 only time counts and at 1 only money. Two hard invariants hold for
every r below 1:

- **Physical fit**: a request never runs on, waits for, or is planned on
  a machine whose physical cores or memory are smaller than the request
  itself while some size or machine of the environment fits it
  physically (overcommit packs several sessions onto a machine; it is no
  reason to run a 5-core build on a 4-core machine). Only when nothing
  fits physically does overcommit admit it.
- **Hedge bound**: no request waits for capacity longer than the fastest
  way to get a machine that fits it (start a stopped machine of its
  environment, create an allowed size on a provider that is up and not
  create-blocked) plus a margin of 30s / (1 - r): 30s at r = 0, 1m at
  0.5, 2.5m at 0.8, unbounded at 1. Waits beyond it are dropped from
  `Place` and from the scale-out timeline, so `Plan` starts or creates
  the machine; the request then takes whatever frees first (a start does
  not commit it). The bound does not apply when no machine can be
  started or created (hard limits, runaway guard without a capacity
  exception, provider outages).

Release ETAs of running sessions are pessimistic (the 80th percentile of
the key's run times; sessions running longer than expected need as long
again), start/boot latencies are learned per provider. Scale-out is a
batch decision: the queue (oldest 16 tasks) is laid out on a timeline of
the existing, starting and candidate machines (earliest completion per
task), every stopped machine of the environment competes with every
allowed size, and a new machine is charged its latency, its runs and its
idle tail until `idle_stop`. Consolidation is that of the cost model
below (forecast, flap guard scaled by r).

The difference to the cost model (`policy = "costmodel"`) is the
scoring: it normalised money and time against the cheapest and fastest
option of each decision, so in small decisions a few cents (3x the cost
of waiting for a share of a busy machine) outweighed minutes of waiting:
in production a build waited 148 s for a warm slot on a busy 4-core x4
while a stopped 8-core x8 of the same environment starts in 16 s, and
then ran with 5 requested cores on the 4-core machine
(`TestRegressionWaitForSmallWarmMachine`); later a warm stopped x8 was
passed over for a cold create of a cheaper OCI size
(`TestStartWarmStoppedOverCheapCreate`). Evaluation: see *Option policy
evaluation* below.

## Cost model (shared candidates; `policy = "costmodel"`)

The cost model scores every candidate of a decision (run now on a
machine, wait for a busy or starting machine, start or create a machine,
prune or grow a full disk, restore a slot snapshot) by money and time to
result and minimises

    r * cost / min_cost + (1 - r) * time / min_time      (r = cost_optimization)

normalised against the cheapest and the fastest candidate of the same
decision. The option policy uses the same candidates, costs and times
with its absolute score and invariants. `cost` is what the pool pays: the machine time a placement
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
  fewer machines win. Sizes that would need more machines than the
  runaway guard still allows lose against sizes that absorb more; a small
  stopped machine is not started when its slot is needed for a larger new
  machine, and an empty running machine is stopped to make room for a larger
  one when the pool is at its guard. Unknown prices (0) prefer the size
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
- **Limits**: the runaway guard and the hard limits (see *Scale-out
  bounds*) also bound starting stopped machines.
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

## Option policy evaluation (Nov 2026)

`internal/scheduler/sim/eval_test.go` replays a fixed evaluation set and
prints a markdown table:

```sh
OFFSHORE_EVAL=default,costmodel,binpack OFFSHORE_EVAL_R=0,0.2,0.5,0.8,1 \
  go test ./internal/scheduler/sim -run TestEvalTable -v     # OFFSHORE_EVAL_CASE=burst filters
OFFSHORE_EVAL_ONE=burst/prod OFFSHORE_EVAL=default OFFSHORE_EVAL_R=0.5 \
  go test ./internal/scheduler/sim -run TestEvalOne -v       # full report of one run
```

Traces: the sample weeks (`team-week`, `small-week`), production exports
(`prod-flap`: evening of 2026-11-10; `prod-nov10`: the old control plane's
316 sessions of 2026-11-10/11; `prod-nov11`: today's 16 sessions with the
waiting regression; `sim export` now joins remote `task_end` events with
their `task_placed` for repository, environment and arrival) and the
adversarial `sim.Scenarios`: `burst` (12 agents every 20 min, idle in
between, so machines are stopped when the next burst comes), `mixed`
(25-40 min 8-core workspace builds among a stream of short checks),
`multienv` (4 environments), `stopped` (sparse builds, a warm stopped
machine per repository, often two at once), `outage` (`burst` with
provider aws out of capacity for an hour) and `disk` (40-80G slots, 25-50G
working sets on local disks, resizes). Pools (`sim.EvalPools`): `c7a`
(testdata/pool.toml, static estimator), `tiers` (c7a with local disks,
history) and `prod` (AWS x4/x8/x16 + OCI e5-8/e5-16, max 8 machines and 4
per provider, idle_stop 1m, history estimator, boot aws 100s / oci 330s,
start 16s / 60s), queue_timeout 10m. Metrics (`sim.Report`): cost, idle
machine-hours (running without a session), turnaround p50/p95 (wait +
run), wait p95, cold builds (neither warm nor restored), flaps, **late**
(waited longer than the fastest start of a stopped machine of the
environment, else the fastest create, at its arrival, + 30s: the SLA of
the hedge bound) and **regret** (hindsight: turnaround minus the best
own-machine alternative at arrival).

Decision-level findings on the cost model (main as of dad5e0c, including
the start-instead-of-wait fix): its normalised score lets small absolute
price differences dominate (late counts grow from 455 at r=0 to 1651 at
0.5 and 4209 at 1 over the set), the three stopped machines it considered
were picked by rendezvous hash rather than price or warmth, new machines
were not charged their idle tail, and single requests ran on machines
with fewer cores than they asked for. Ablations of the option policy
(geomean over the set): the absolute score with the cheapest size as the
time reference beat the most expensive size (0.97 vs 1.02 cost, 0.91 vs
0.90 p95 against the pre-fix cost model) and scaled references of
0.5-5x; the physical-fit invariant costs about 1 % and saves 4 % of p95.

Summary over all 21 cases (geomean ratios against the cost model at the
same r):

| policy | geomean cost vs costmodel | geomean turn p95 vs costmodel | wait p95 (median) | late (sum) | idle h (sum) | cold (mean) | failed |
|---|---:|---:|---:|---:|---:|---:|---:|
| costmodel r=0.0 | 1 | 1 | 60s | 455 | 431 | 15.3% | 10 |
| costmodel r=0.2 | 1 | 1 | 71s | 851 | 465 | 16.7% | 7 |
| costmodel r=0.5 | 1 | 1 | 100s | 1651 | 494 | 15.9% | 11 |
| costmodel r=0.8 | 1 | 1 | 143s | 2466 | 434 | 17.9% | 7 |
| costmodel r=1.0 | 1 | 1 | 284s | 4209 | 372 | 18.7% | 18 |
| binpack | 1 | 1 | 60s | 355 | 482 | 18.8% | 10 |
| default r=0.0 | 0.993 | 1.027 | 60s | 360 | 471 | 15.7% | 6 |
| default r=0.2 | 0.955 | 0.945 | 60s | 486 | 496 | 17.0% | 8 |
| default r=0.5 | 0.939 | 0.939 | 60s | 957 | 468 | 17.2% | 10 |
| default r=0.8 | 0.916 | 0.932 | 119s | 2152 | 369 | 17.2% | 10 |
| default r=1.0 | 0.868 | 1.005 | 284s | 4405 | 261 | 17.1% | 11 |

At the default and the production setting the option policy is cheaper
and faster than the cost model (r=0.5: -6 % cost, -6 % p95, 42 % fewer
late requests; r=0.2: -5 %, -6 %, -43 %); at r=1 it is 13 % cheaper.
Binpack remains the cheapest point on several traces of the production
pool (it overcommits single requests and consolidates on the present
reservation) at a higher p95; the option policy matches or beats 84 of the
126 cost model and binpack points of the set within 2 % in both cost and
p95. Per case (cost / turnaround p95 / late):

| case | costmodel r=0.2 | default r=0.2 | costmodel r=0.5 | default r=0.5 | binpack |
|---|---|---|---|---|---|
| team-week/c7a | $75.14 / 9m13s / 164 | $67.61 / 10m20s / 133 | $74.51 / 9m27s / 455 | $66.47 / 10m28s / 220 | $60.03 / 13m36s / 12 |
| team-week/prod | $33.00 / 12m22s / 324 | $35.61 / 9m52s / 95 | $30.60 / 16m25s / 537 | $30.50 / 14m5s / 331 | $30.33 / 13m1s / 225 |
| small-week/c7a | $23.25 / 6m33s / 36 | $23.48 / 7m0s / 30 | $22.66 / 6m52s / 90 | $22.88 / 7m0s / 45 | $17.92 / 9m42s / 3 |
| small-week/prod | $11.63 / 6m19s / 39 | $11.24 / 6m24s / 43 | $10.98 / 6m29s / 86 | $10.62 / 6m10s / 53 | $8.41 / 7m14s / 10 |
| prod-flap/c7a | $1.76 / 3m9s / 0 | $1.66 / 3m9s / 0 | $1.66 / 3m9s / 1 | $1.66 / 3m9s / 0 | $1.27 / 3m9s / 1 |
| prod-flap/prod | $0.99 / 4m37s / 4 | $0.75 / 3m51s / 4 | $0.63 / 4m45s / 6 | $0.40 / 3m54s / 8 | $0.32 / 4m2s / 2 |
| prod-nov10/c7a | $9.91 / 3m45s / 1 | $9.76 / 3m45s / 0 | $9.71 / 3m45s / 2 | $9.68 / 3m45s / 0 | $8.63 / 3m38s / 2 |
| prod-nov10/prod | $3.91 / 3m54s / 28 | $3.64 / 3m53s / 42 | $2.18 / 4m4s / 54 | $1.82 / 3m51s / 60 | $1.59 / 3m51s / 2 |
| prod-nov11/c7a | $0.76 / 6m45s / 0 | $0.72 / 6m45s / 0 | $0.72 / 6m45s / 0 | $0.72 / 6m45s / 0 | $0.69 / 5m57s / 0 |
| prod-nov11/prod | $0.36 / 11m14s / 7 | $0.53 / 11m25s / 6 | $0.29 / 13m51s / 7 | $0.48 / 10m49s / 6 | $0.23 / 12m11s / 3 |
| burst/c7a | $7.96 / 4m57s / 6 | $4.98 / 5m20s / 0 | $4.98 / 4m48s / 18 | $4.98 / 5m20s / 0 | $4.20 / 6m52s / 0 |
| burst/prod | $1.06 / 6m57s / 39 | $1.05 / 6m33s / 24 | $1.13 / 6m35s / 48 | $1.06 / 7m5s / 50 | $0.56 / 8m56s / 12 |
| mixed/c7a | $5.52 / 2m6s / 2 | $5.70 / 2m7s / 1 | $5.59 / 2m18s / 4 | $5.61 / 2m15s / 1 | $5.24 / 2m31s / 0 |
| mixed/prod | $4.08 / 6m20s / 48 | $4.37 / 3m14s / 2 | $5.75 / 6m39s / 97 | $2.79 / 3m44s / 12 | $2.14 / 4m43s / 8 |
| multienv/c7a | $17.89 / 13m32s / 19 | $17.08 / 14m13s / 15 | $17.42 / 14m17s / 71 | $16.78 / 14m17s / 30 | $15.53 / 16m36s / 0 |
| multienv/prod | $8.65 / 22m51s / 71 | $11.03 / 18m18s / 65 | $8.28 / 23m20s / 95 | $8.79 / 23m31s / 89 | $9.33 / 25m29s / 62 |
| stopped/c7a | $2.32 / 7m39s / 0 | $2.32 / 7m39s / 0 | $2.28 / 7m39s / 3 | $2.32 / 7m39s / 0 | $1.91 / 7m39s / 0 |
| stopped/prod | $1.09 / 11m18s / 8 | $1.01 / 8m19s / 0 | $1.07 / 11m18s / 9 | $1.02 / 8m19s / 1 | $0.72 / 8m29s / 1 |
| outage/c7a | $8.22 / 4m46s / 7 | $4.98 / 5m8s / 0 | $4.98 / 4m48s / 12 | $4.98 / 5m8s / 0 | $4.20 / 6m52s / 0 |
| outage/prod | $1.06 / 6m57s / 39 | $1.05 / 6m33s / 24 | $1.13 / 6m35s / 47 | $1.06 / 7m5s / 50 | $0.56 / 8m56s / 12 |
| disk/tiers | $4.88 / 12m57s / 9 | $4.64 / 14m34s / 2 | $5.03 / 13m17s / 9 | $4.18 / 11m21s / 1 | $4.28 / 17m19s / 0 |

`TestRegressionWaitForSmallWarmMachine`, `TestStartWarmStoppedOverCheapCreate`,
`TestPhysicalFit`, `TestHedgeBound` and `TestTimeValue` pin the invariants
and the production regressions; `TestCheapestBeatsBinpack` now allows r=1
up to 10 % above binpack (the price of the physical-fit invariant on the
static c7a traces), and `TestThrashingPicksBiggerSize` expects the bigger
size up to r = 0.3 (8 minutes of thrashing against twice the price).

## Round 2: no machine limits, projected-start gate, experiments (Nov 2026)

The production pool of 2026-11-12 in the simulator (`sim.EvalPools`
`prod`: AWS x8 8 cores/30G $0.58, x16 16/30G $0.98, OCI disabled,
idle_stop 1m) and the c7a sample pool, without any machine limit (the old
limits: `OFFSHORE_EVAL_LIMIT=aws:4`). Ratios are geometric means over the
cases at the same r; *failed* are requests that timed out in the queue.

**Today (option policy, AWS at most 4 machines) → round 2 (default, no
limit)**, prod pool, 11 cases:

| r | cost | idle h | turn p95 | late | failed | max running (sum / max) |
|---|---:|---:|---:|---:|---:|---:|
| 0.0 | +1.0 % | +6.0 % | -3.3 % | 217 → 27 | 73 → 0 | 27 → 46 / 4 → 20 |
| 0.2 | +0.8 % | +6.5 % | -0.9 % | 268 → 32 | 66 → 0 | 28 → 46 / 4 → 20 |
| 0.5 | +1.0 % | +4.1 % | -4.2 % | 444 → 60 | 71 → 0 | 28 → 40 / 4 → 12 |
| 0.8 | +1.4 % | +5.0 % | -3.9 % | 532 → 183 | 80 → 0 | 28 → 36 / 4 → 8 |
| 1.0 | +1.5 % | +7.7 % | -3.5 % | 775 → 674 | 107 → 0 | 24 → 31 / 4 → 8 |

Per case at r = 0.2 (cost / idle h / turn p95 / late / failed / max running):

| case | today | round 2 |
|---|---|---|
| team-week/prod | $66.75 / 13.9 / 12m31s / 143 / 12 / 4 | $67.08 / 14.2 / 12m19s / 23 / 0 / 7 |
| small-week/prod | $23.16 / 9.5 / 5m54s / 4 / 0 / 4 | $23.19 / 9.5 / 6m21s / 4 / 0 / 3 |
| prod-nov10/prod | $4.54 / 1.8 / 3m45s / 0 / 0 / 2 | $4.54 / 1.8 / 3m45s / 0 / 0 / 2 |
| prod-nov11/prod | $0.54 / 0.1 / 8m21s / 0 / 0 / 1 | $0.54 / 0.1 / 8m21s / 0 / 0 / 1 |
| burst/prod | $1.59 / 0.5 / 6m59s / 0 / 0 / 3 | $1.59 / 0.5 / 7m8s / 0 / 0 / 3 |
| multienv/prod | $16.02 / 1.4 / 25m56s / 121 / 54 / 4 | $17.46 / 2.8 / 21m16s / 5 / 0 / 20 |
| warmbusy/prod | $2.52 / 0.5 / 32m26s / 0 / 0 / 1 | $2.52 / 0.5 / 32m26s / 0 / 0 / 1 |

The production traces (prod-flap, nov10, nov11) never reached the limit
and are unchanged; the limit only cost waiting and timeouts where demand
exceeded four machines (multienv: four environments at once). On the c7a
pool, today's option policy with its old `max_machines = 16` against
round 2 without limit: cost +0.0..1.2 %, p95 -0.7..-0.1 %, failed 10 → 4
at r = 0.2.

**Ablations** (default with one part removed, or an experiment added;
all 23 cases, r = 0.2 / 0.5 / 0.8):

| part | cost | turn p95 | other | kept |
|---|---:|---:|---|---|
| projected-start gate (`nogate`) | ±0.1 % | ±0.2 % | late 232 → 225 / 431 → 441 | yes: the provisioning rule that replaces the machine limit, neutral |
| stop decay, one idle stop per env per minute (`decay`) | +0.5 % | ±0.3 % | idle h +2 % | no (experiment) |
| batch placement, SRPT + aging + owner round robin (`batch`) | ±0.1 % | -0.9..+0.8 % | late 1496 → 1546 without at 0.8 | no (experiment) |
| remaining-time distribution of waits (`dist`) | ±0.3 % | ±0.4 % | OOMs 15 → 25 at r = 0.8 | no (experiment) |
| statistical memory admission, Σmean + z·√Σvar (`stat`) | ±0.1 % | +1.3..2.4 % | OOMs unchanged | no (experiment) |

`OFFSHORE_EXPERIMENT=batch,regret,dist,stat,decay` turns the experiments
on (simulator only), `OFFSHORE_ABLATE=nogate` turns the gate off.
Statistical admission is never stricter than the reservations: an early
version refused what the reservations admitted and stalled the queue
(`TestSampleTraces` at r = 1 ran for minutes). The second warm copy's
disk cost (restore while the warm machine keeps its slot) and the
writeback snapshots stay in the model. Round 2 default against round 1
(`policy = "option"`), both without limits: cost -0.5..+0.6 %, p95
-0.6..+0.5 %, failed 6 → 4 at r = 0.5. Against the cost model: cost
-1.4..-10.8 %, p95 -5.2 % at r ≥ 0.5, late 1240 → 462 at r = 0.5. Against
binpack: +9 % cost / -8 % p95 at r = 0.2, -8 % cost / +19 % p95 at r = 1.

Warm but busy (`warmbusy` scenario, `TestWarmBusyTradeoff`): a request
waits for its warm machine when it frees within the hedge bound and
otherwise restores its slot on another running machine of the
environment; the restore is charged the egress and the second warm copy's
disk. On the prod pool all policies give the same result ($2.52, p95
32m26s: the long builds dominate); on c7a r = 0.2 round 2 is $3.20 vs
$3.45.

Diagnosis of "3 cores/3G waited although x8 had room" (2026-11-11): at
13:09 the request was assigned in 74 ms to a warm slot. The wait at
13:12-13:13 came from a `cargo check` request that reserved 11.4G: its
key had learned the peak of a clippy run sharing the same usage key, so
the x8 was reserved for 8 cores and the 3-core request did not fit. Usage
keys are now scoped by package (`?p=a,b`, `?workspace`), `offshore fetch`
has its own key with a small need, and keys without history fall back to
their parent key (`learned_parent`).

**Follow-ups** (not implemented):

- *Work fraction*: a seam for change impact: the client sends the paths
  changed since the slot's base (`git diff --name-only slot_base..HEAD`
  plus pseudo-inputs such as toolchain and lockfile) with the request; the
  worker labels usage with the observed fraction of work done (CPU seconds
  between the no-op floor and the cold ceiling of the key), and the
  estimator learns run time and memory as a function of it.
- *Runtime safety* on shared machines: a machine-wide jobserver shared by
  the sessions (cargo/make honour `MAKEFLAGS`/`CARGO_MAKEFLAGS`), a link
  pool limiting concurrent rustc links (the memory peaks), zram as a
  compressed swap tier instead of OOM kills, and freezing the newest
  session (cgroup freezer) instead of killing it on memory pressure. All
  four are feasible on the worker; the first two need a token server in
  the supervisor.
- OCI quota discovery (Limits API `GetResourceAvailability` per shape
  family and availability domain): the scheduler's limit is one vCPU
  figure per provider while OCI limits are per shape family, so only the
  limit-error cap applies there.

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
