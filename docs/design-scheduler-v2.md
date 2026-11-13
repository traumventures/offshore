# Scheduler v2: one decision model

Status: implemented (`internal/scheduler/core.go`, `lifecycle.go`), the
only policy. Replaces the option policy, the cost model and the
layers added on top of them (runaway guard, capacity exception,
capQueuedDemand, projected-start gate, queue verdicts, consolidation,
flap guard, stop decay, batch/dist/stat experiments).

## Decision model

Every event (arrival, finish, machine ready, the 15 s tick) runs one
*solve*: the pending requests in priority order (shortest predicted run
first, aged by waiting, owners round robin), at most 32, each choosing the
option with the least

    expected cost + time_value × expected time to result
    time_value = price of the cheapest size × (1 − r) / r,  r = cost_optimization

over an explicit option set:

| option | time | cost |
|---|---|---|
| run on machine M now (warm slot, sibling seed, restore, move or cold; the reclaim or resize it needs is part of the option) | run (+ restore/reclaim/resize, thrash) | M's marginal price for the run + egress + extra disk + the slowdown it causes the sessions already on M (overcommitted cores) |
| wait for M (busy, starting, or brought by an earlier request of this solve) | wait at the queue_confidence quantile (0.8) of the sessions' remaining times + run | the same, from M's free time |
| start stopped machine S | start latency + run | price × (latency + run + idle tail) |
| create size Z (or attach a warm detached volume) | boot (+ attach) + run | price × (boot + run + idle tail) |

The chosen option is *booked* on the machine's timeline. A later request
of the same solve sees the capacity taken, including machines that are
starting or that this solve starts or creates. That is the whole
counting mechanism: there is no runaway guard, no per-request exception,
no cap on queued demand and no projected-start gate. Two bounds remain
because they express the objective's own edges, not tuning: a wait longer
than the fastest start/create plus 30 s/(1−r), counting the time already
waited, is not an option (ski rental: a release that keeps overrunning
keeps looking a minute away, so past the bound the request takes new
capacity), and a
wait past 80 % of `queue_timeout` only when nothing else is left. After
the solve, creates of one environment are merged into fewer larger
machines when that scores better (each request picked the cheapest
machine for itself). Place runs the same solve and assigns the request if
its best option is a machine with room now; Plan executes the starts and
creates the solve booked.

Hard limits are the only filters: providers with `enabled = false`,
account limits (EC2 vCPU quota, observed limit errors), `[pool]
max_spend_per_hour`. They remove options; nothing lifts them.

## Machine lifecycle (the 0–1 machine regime)

Idle running and stopped machines are a rent-or-buy (ski-rental) problem.
The learning store records request arrivals per environment
(`learning.json`, last 512). The k-th machine of an environment (busy
ones first, cheapest idle next) is needed again at the next arrival that
has k−1 others within 5 minutes before it; the gaps between such events,
restricted to those longer than the time already idle (and to a similar
time of day when there are enough), are the distribution of when.

- idle running → stop: keep at price/h vs. a later start costing
  (time_value + price) × start latency (16 s on EC2). Keep exactly while
  some further keeping time has a lower expected cost than stopping now;
  at least 1 minute (EC2 bills a minute per start). Without history the
  deterministic break-even (penalty / price) applies. `idle_stop`, when
  set, overrides this with a fixed time (and disables destroys).
- stopped → destroy: keep the disk (EBS $/GB-h) vs. a later create instead
  of a start ((time_value + price) × (boot − start) + 10 min of cold
  builds while the warm slots restore).
  The first machine of an environment is never destroyed early.
- renew: when the last machine of an environment reaches max_lifetime and
  the environment was used in the last day, a replacement is created (it
  boots, idles a minute and stops) when the create–start difference at
  time_value is worth more than its boot, minute and disk until the next
  expected request: the next first session starts, not boots.
- warm pool (`min_machines`, `[[warm_pool]]`) stays a floor.

## Storage invariant

The store head of a slot is the source of truth; everything on a machine
(slot pages, materialized files, seeds, fetched sources, machine caches)
is a reclaimable tier. The worker reports a physical ledger of its disk
and reclaims in one order (cost per byte); the scheduler prices disk
only as option cost (reclaim, resize, restore). See
[design-storage-v2.md](design-storage-v2.md).

## What the scheduler reads

Only the pool snapshot and the learning store (usage per key, refetch
factors, arrivals). The event log is observability only.

## Evaluation

`TestGate` against the scheduler before the rewrite, 31 cases × r ∈
{0.2, 0.5, 0.8}: cost −23..−25 % at high load, −0.5..−3 % at low load;
objective (cost + time_value × Σ turnaround) lower or equal everywhere;
p95 turnaround −4.0 % / −0.8 % at high load for r = 0.2 / 0.8 (+1.3 % at
0.5), equal at low load.
Details in [scheduling.md](scheduling.md#v2-gate-nov-2026).
