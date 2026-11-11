# Scheduler

The scheduler (`internal/scheduler`) decides where a session runs (`Place`)
and how the pool changes (`Plan`) as pure functions of a pool snapshot.
The orchestrator only gathers state and executes the decisions, so policies
can be tested and simulated without machines.

## Default policy

`pool.policy = "default"` (alias `"binpack"`):

- **Placement**: machines with a warm build cache for the repository come
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
  gets the cheapest fitting size; several queued tasks may justify one larger
  machine when it is cheaper per unit, and at equal cost per unit fewer
  machines win. Sizes that would need more machines than `max_machines`
  still allows lose against sizes that absorb more; a small stopped machine
  is not started when its slot is needed for a larger new machine, and an
  empty running machine is stopped to make room for a larger one when the
  pool is at its limit. Unknown prices (0) prefer the size absorbing the
  most demand, then the smaller one.
- **Architecture**: machines are only created from sizes of the
  environment's architecture (the repository's `resources.arch`; unknown
  environments are amd64), and a task no size of its architecture can hold
  is rejected.
- **Consolidation**: when the machines of an environment are reserved below
  `consolidate_below` (default 0.5) and the others stay below
  `consolidate_target` (default 0.75) after taking its share, the least
  loaded busy machine is drained (one at a time, never below the warm pool,
  never while tasks of the environment wait). New work lands elsewhere and
  the drained machine stops as soon as its sessions end. When demand returns
  it is undrained before anything is started or created.
- **Idle stop and warm pool**: idle machines stop after `idle_stop` unless
  the warm pool keeps them: `min_machines` per environment used in the last
  24 hours, raised by scheduled windows:

  ```toml

[scheduler]

[[scheduler.warm_pool]]
  days = ["mon", "tue", "wed", "thu", "fri"]   # empty = every day
  from = "08:00"
  to = "19:00"                                # earlier than from spans midnight
  machines = 1                                # per recently used environment
  timezone = "Europe/Berlin"                  # default: local time
  ```

- **Limits**: `max_machines` now also bounds starting stopped machines (the
  previous policy could exceed it).

`pool.policy = "classic"` keeps the previous behaviour (rendezvous
placement, cheapest size for the aggregate demand, no consolidation) for
comparison.

New keys: `policy = "default" | "binpack" | "classic"`, `consolidate_below`
(0 = off), `consolidate_target`, `[[scheduler.warm_pool]]`.

## Simulator

`cloudsourced sim` replays traces against the pool configuration:

```sh
cloudsourced sim -config internal/scheduler/sim/testdata/pool.toml \
  internal/scheduler/sim/testdata/team-week.json internal/scheduler/sim/testdata/small-week.json
cloudsourced sim -generate trace.json -profile team -seed 1 -days 5 -tz Europe/Berlin
```

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

`cloudsourced report` summarises a window from it:

```sh
cloudsourced report --since 7d          # or --since 2026-11-01 --until 2026-11-08, --json
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

`cloudsourced sim export` turns the finished sessions into a trace the
simulator replays:

```sh
cloudsourced sim export --since 7d -o real-week.json
cloudsourced sim -config pool-candidate.toml -policy default,classic real-week.json
cloudsourced sim -config pool-candidate.toml -estimator history real-week.json
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
check that `cloudsourced sim` with the current pool configuration
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
