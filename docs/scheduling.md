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
  with free capacity: warm slot, restore of the best snapshot, cold; each
  after prune or resize if the disk is too full) and every way to wait
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
- **Idle stop**: an emptied consolidated machine is undrained when the
  others cannot take the forecast demand.

The orchestrator feeds the model: `Task.Profile` (estimator), expected
releases (lease start + placement's expected run), provider latencies and
cooldowns, snapshots (`snapshots.json` in the state dir), machine disk and
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
(`DiskTotal`, `DiskUsed`) and the size of each repository's build slot
(`Status.Disk`), and the snapshots held in object stores
(`Status.Snapshots`). Sizes may declare `disk`. Sessions report their slot
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
- the estimator (`-estimator static|history`) learns from runs and OOMs.

Traces are JSON (`{"start": RFC3339, "end": seconds, "jobs": [{"at",
"repo", "env", "duration", "cpu", "memory", "peak_memory"}]}`) or CSV with
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
