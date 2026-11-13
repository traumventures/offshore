# Scheduling

The scheduler (`internal/scheduler`) decides where a session runs (`Place`)
and how the pool changes (`Plan`) as pure functions of a pool snapshot
and the learning store (interfaces `Policy` and `Estimator`). The
orchestrator only gathers state and executes the decisions, so the
scheduler can be tested and simulated without machines. One orchestrator
can span several providers; sizes of all providers compete.

The decision model is described in
[design-scheduler-v2.md](design-scheduler-v2.md): one option model for
placement and scale-out (`core.go`), a rent-or-buy lifecycle for idle and
stopped machines (`lifecycle.go`), hard limits only (`limits.go`), the
option costs of run, restore, move, reclaim and resize (`model.go`,
`tiers.go`) against the worker's disk ledger
([design-storage-v2.md](design-storage-v2.md), `ledger.go`).

## Configuration

```toml
[scheduler]
cost_optimization   = 0.5         # 1 = cheapest, 0 = fastest result
queue_confidence    = 0.8         # waits are scored at this quantile of the remaining run times
disk_price_per_gb_month = 0.08    # prices disk resizes
disk_budget    = "90%"            # the rest of the cache disk is the worker's minimum reserve ("150G": bytes)
source_cache   = "12.5%"          # fetched source contents kept, share of the disk budget ("20G": bytes)
estimator           = "history"   # history | static
cpu_overcommit      = 2.0         # only slows builds down
memory_overcommit   = 1.25        # requires soft_limit or swap, otherwise OOM
soft_limit          = 1.2         # memory.high = request * 1.2
swap                = true
max_cpu_pressure    = 0.0         # PSI some avg10; no new sessions above (0 = off)
max_memory_pressure = 0.4
limit_jobs          = true        # CARGO_BUILD_JOBS = granted cores

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
| `cost_optimization` | 0.5 | 0..1, the single trade-off knob: an hour of turnaround is worth (1-r)/r hours of the cheapest size; 1 = cheapest, 0 = fastest result |
| `queue_confidence` | 0.8 | a wait for a busy or starting machine is scored with its sessions ending at this quantile of their remaining times (waiting wins only when it finishes sooner with this probability); sessions without a known end are not counted on to end |
| `policy` | `"default"` | the only value; the earlier policies (`option`, `costmodel`, `binpack`, `classic`) were removed |
| `[[egress]]` | free within a provider, 0.09/GB at 50 MB/s across | `from` (provider or `storage`), `to` (provider), `price_per_gb`, `bandwidth` |
| `disk_price_per_gb_month` | 0.08 | price of extra disk and of stopped machines' disks (resize, destroy decisions) |
| `source_cache` | `"12.5%"` | source contents a worker fetched from the laptop are kept as a cache, reclaimed least recently used beyond this share of the disk budget or size (`"20G"`); open files never |
| `disk_budget` | `"90%"` | how much of a machine's cache file system data may fill: the rest (at least 5 GB) is the minimum of the worker's reserve, the headroom it keeps free beyond the forecast growth of the running sessions; a percentage or a size (`"150G"`); sizes override it with their own `disk_budget` |
| `estimator` | `"history"` | `history` learns CPU (average busy cores, capped by the configured value) and memory (p90 of the peak plus 25 %; before 3 runs the configured value raised to the largest observed peak plus 25 %) per repository/tool/subcommand from cgroup usage; `static` uses `[resources]` |
| `cpu_overcommit` | 2 | reserved CPU may exceed physical cores by this factor (>= 1) |
| `memory_overcommit` | 1 | > 1 needs `swap = true` or `soft_limit` |
| `soft_limit` | 0 (off) | sets `memory.high` to request x factor |
| `swap` | false | allow sessions to swap |
| `max_cpu_pressure`, `max_memory_pressure` | 0 (off) | PSI threshold 0..1 above which a machine takes no new sessions |
| `limit_jobs` | false | pass the reserved cores as build parallelism (`CARGO_BUILD_JOBS`, `OFFSHORE_JOBS`) |
| `[[warm_pool]]` | none | scheduled warm machines in addition to `pool.min_machines` |

Removed in v2: `consolidate_below`, `consolidate_target` (no
consolidation: idle machines stop by the lifecycle rule, so lightly used
machines empty and stop on their own) and the `policy` values other than
`default`. Both are rejected by the config loader.

Memory is never overcommitted beyond `memory_overcommit`; CPU is. Lifetimes
(`idle_stop`, `max_lifetime`), `min_machines`, `max_spend_per_hour` and
`queue_timeout` are in `[pool]` ([configuration.md](configuration.md#pool-configuration)).
`idle_stop` is unset by default: the lifecycle policy derives when an idle
machine stops (and when a stopped one is destroyed) from the learned
arrivals, prices and latencies; a configured `idle_stop` overrides it with
a fixed time (machines then stop after it and stopped machines are only
destroyed at `max_lifetime`). Unobserved machines (no orchestrator) stop
themselves after `idle_stop`, 20 minutes when unset.

## Scale-out bounds

There is no configured machine count and no runaway guard. The solve
books every request on a machine (existing, starting, or one it starts or
creates for an earlier request), so a request never gets a second machine
while one is on its way, and a start or create is only chosen when it
scores better than every wait. Hard limits are enforced on every plan
(`enforceLimits`): providers with `enabled = false`, the provider
accounts' limits (`Pool.ProviderLimit`) and the optional `[pool]
max_spend_per_hour` (hourly price of running and starting machines).
Account limits are discovered: aws-ec2 reads the On-Demand standard vCPU
quota (Service Quotas `L-1216C47A`) every 10 minutes and subtracts the
account's other running instances; any provider that answers a create
with a quota/limit error is capped at the vCPUs the pool runs there for an
hour (not disabled, no cooldown; without a machine there it cools down as
before). Account-type limits (free tier) and auth errors still disable the
provider.

Every decision is recorded (`Decision` on placements, starts and creates
in the event log, field `decision`): each option with its time, cost and
score, the chosen one, and the options rejected before scoring with the
reason (machine smaller than the request, wait longer than the fastest
start/create plus 30s/(1-r) counting the time already waited, past 80 % of `queue_timeout`, provider down,
create backoff, hard limit).

## Inputs: disk, slots and snapshots

The pool snapshot the policy sees contains, per machine, the disk
(`DiskTotal`, `DiskUsed`, the worker's `Budget`) and the size of each
repository's build slot (`Status.Disk`), and the snapshots held in object
stores (`Status.Heads`, plus those the orchestrator saved, each recorded
with the provider of the machine it came from). Sizes may declare `disk`. Sessions report their slot
size before and after the run (`task_end.usage.slot_bytes_before/after`).
Workers report a physical ledger of their disk (`Status.Disk.Ledger`:
free, pinned, reserve and what is reclaimable at which cost); a placement
that needs room reclaims (`SlotAction.Reclaim`) or grows the disk
(`SlotAction.Grow`), a machine that cannot make room is no option, and
disks whose pinned data and reserve exceed 60 % grow in the background
(`Grow`). See [design-storage-v2.md](design-storage-v2.md) and
[caches-and-storage.md](caches-and-storage.md).

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

## v2 gate (Nov 2026)

`TestGate` (`internal/scheduler/sim/gate_test.go`) replays 31 cases (the
sample weeks, the production exports prod-flap/nov10/nov11/nov11-day/
nov12, the adversarial scenarios, and synthetic single-developer weeks
`solo` and `sparse`) on the c7a sample pool and the production pool (x8
$0.58 / x16 $0.98, start 16 s, create 100 s, gp3 disks charged also while
stopped; history estimator in both), at cost_optimization 0.2, 0.5 and
0.8. The scheduler before the rewrite (commit 8336677, `idle_stop` as
configured: 20m on c7a, 1m on prod) is replayed from
`testdata/gate-base.json`; v2 derives `idle_stop`. Low load: the old
scheduler never ran more than two machines.

| group | policy | cost $ | $/build | turn p95 vs first (geo) | first wait p95 (mean) | objective $ |
|---|---|---:|---:|---:|---:|---:|
| high r=0.2 | old | 345.10 | 0.026 | 1.000 | 74s | 1477 |
| high r=0.2 | v2 | 261.59 | 0.020 | 0.960 | 65s | 1306 |
| high r=0.5 | old | 352.80 | 0.026 | 1.000 | 72s | 652 |
| high r=0.5 | v2 | 262.31 | 0.019 | 1.013 | 67s | 557 |
| high r=0.8 | old | 324.52 | 0.024 | 1.000 | 78s | 400 |
| high r=0.8 | v2 | 248.87 | 0.019 | 0.992 | 64s | 323 |
| low r=0.2 | old | 29.26 | 0.031 | 1.000 | 52s | 120 |
| low r=0.2 | v2 | 28.76 | 0.030 | 1.000 | 52s | 120 |
| low r=0.5 | old | 18.14 | 0.028 | 1.000 | 58s | 31 |
| low r=0.5 | v2 | 18.05 | 0.028 | 0.999 | 58s | 31 |
| low r=0.8 | old | 29.15 | 0.031 | 1.000 | 52s | 35 |
| low r=0.8 | v2 | 28.28 | 0.030 | 1.001 | 52s | 34 |

Selected cases (cost $, turnaround p95, first-session wait p95), old → v2:

| case | r | cost | turn p95 | first wait p95 |
|---|---|---|---|---|
| solo/c7a (default idle_stop 20m) | 0.2 | 13.83 → 4.10 | 5m29s → 5m30s | 1m0s → 20s |
| sparse/c7a | 0.2 | 4.05 → 1.07 | 7m5s → 7m8s | 1m0s → 1m0s |
| solo/prod (idle_stop 1m) | 0.2 | 11.13 → 10.62 | 6m25s → 6m25s | 16s → 16s |
| team-week/prod | 0.2 | 82.42 → 80.69 | 11m46s → 10m47s | 16s → 16s |
| prod-nov12/prod | 0.2 | 5.36 → 4.39 | 5m50s → 5m0s | 1m40s → 1m40s |
| burst/c7a | 0.8 | 4.22 → 1.28 | 5m54s → 6m25s | 1m0s → 1m0s |
| stopped/c7a | 0.8 | 2.10 → 0.89 | 7m53s → 8m39s | 1m0s → 20s |
| disk/tiers | 0.8 | 3.64 → 3.82 | 12m55s → 15m49s | 1m0s → 2m0s |

Result: v2 is cheaper in every group (−23 to −25 % at high load, −0.5 to
−3 % at low load) and has the lower or equal objective everywhere.
Turnaround p95 is better at r = 0.2 and 0.8 at high load (−4.0 %, −0.8 %),
+1.3 % at r = 0.5, and equal at low load (the +0.1 % at r = 0.8 is one
second on one case, prod-nov10/prod 3m45s → 3m46s). The worst single case
is disk/tiers (local-disk sizes): p95 +22 % at r = 0.8. In the 0–1
machine regime the derived lifecycle cuts cost per build by 60–75 %
against the default `idle_stop = 20m` with a first session that starts a
stopped machine (20 s) instead of creating one; against production's
hand-tuned `idle_stop = 1m` it is equal on first-session wait and 1–8 %
cheaper.

objective = cost + time_value x the sum of all turnarounds, the quantity
both schedulers minimise at that cost_optimization. Run it with

```sh
OFFSHORE_GATE=v2 OFFSHORE_GATE_BASE=testdata/gate-base.json \
  go test ./internal/scheduler/sim/ -run TestGate -v
```

With storage v2 (the physical disk model of the simulator, the ledger in
the scheduler) the same gate gives, against the scheduler before v2: cost
−25 % / −24 % at high load (r = 0.2 / 0.8), turnaround p95 −6.0 % / −3.9 %
(geometric mean), objective −15 % / −21 %; low load −1 to −3 % cost,
p95 −0.6 to −1.5 %. Storage-specific results (warm starts, ENOSPC, disk
waits) are in [design-storage-v2.md](design-storage-v2.md#gate-simstorage_gate_testgo).

## Learning store

What the scheduler learns from the past lives in a dedicated store,
`internal/learning` (`learning.json` in the state directory, schema
`version` 1, other versions are ignored and relearned): the estimator's
per-key usage window (run time, warm/cold/restore, peak memory, slot
growth, working set, refaults; 20 runs per key, at most 4096 keys, least
recently updated dropped), the refetch factor per provider and the
last 512 request arrivals per environment (the lifecycle policy). The
orchestrator writes it as sessions end (`History.Observe`) and as workers
report cache observations (`Refetch`); the estimator and the policy read
it (`Profile`, `Pool.Refetch`, `Pool.Arrivals`, run-time distributions of `Release`). The
estimator's former `usage-history.json` is imported once.

The event log (`events-*.jsonl`) is for observability only (humans,
agents, `offshore report`, simulator traces via `offshore sim export`);
the scheduler never reads it. Earlier the refetch calibration replayed
the last 24 h of event files at start and tapped the log; both are gone
(`events.Log.Tap` removed): the orchestrator's `emit` sends each
observation to the two sinks independently.

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
offshore sim -config pool-candidate.toml -cost-optimization 0.2,0.8 real-week.json
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
overcommit factors and cost_optimization values on the same trace.

## Memory safety (incident 2026-11-12)

Three clippy sessions of new worktrees reserved the configured 3G each on
a 15G x8s machine, peaked at 4.5-6.5G each and the host ran out of
memory; the machine stayed unreachable and requests queued behind it.

- **Admission by learned peak.** `Estimate.LearnedPeak` is the key's p90
  peak, and while a key has fewer than `MinSamples` runs also the largest
  peak of its own runs, its parent keys and the same command in other
  repositories (worktrees). The control plane raises the task's memory
  reservation to `LearnedPeak x memory_overcommit` (capped by
  `max_memory`), so the learned peaks of a machine's sessions never sum
  above its memory.
- **Sessions cannot take the host down.** Session cgroups live in
  `offshore-sessions`, capped at machine memory minus 1.5G; the OOM killer
  acts there (reported as the session's OOM, the retry reserves more).
  Worker and supervisor run at `oom_score_adj -1000`, sessions at 500.
- **Unreachable machines.** A machine whose worker was ready and has not
  answered a probe for 2 minutes is no capacity (`Gone` for the
  scheduler; `unreachable` in top and status). While requests wait for
  its environment it is rebooted at the provider (aws-ec2), and replaced
  when it still does not answer 3 minutes later, also as the only machine
  of its provider.

## Open issues

- The orchestrator drops environments from its recent list after 24 hours,
  so a scheduled warm pool only applies to environments used in the last
  day (on Monday morning after a quiet weekend no machine is pre-started).
- EBS/AMI warm-up effects after a start are not modelled (EC2's 60 s
  minimum billing per start is).
- disk/tiers (sizes with local disks) was the one gate case clearly
  slower after scheduler v2; with storage v2 it runs without ENOSPC (the
  physical disk model showed 74-170 before) at half the cost, see
  `sim/testdata/storage-gate-v2.md`.
- The simulator assumes peak memory for the whole run and constant CPU use;
  real builds have phases.
- `sim export` cannot tell a build that was slowed by CPU contention from
  one that is not parallel: both appear as fewer busy cores for longer. The
  event log has no per-session pressure yet.
