# Operations

## Status, why and doctor

```sh
offshore status [--json]     # machines, sessions (with labels), cost today
offshore why [N|last|list]   # last run: local/remote and why, adapter and phases, machine,
                                 # placement, timings, usage, cache counters, labels (--json)
offshore doctor [--json]     # installation, configuration and connectivity
offshore top                 # live view
offshore report --since 7d   # cost, utilisation, queue wait, caches, estimator accuracy
offshore metrics             # Prometheus text
```

`offshore why` reads the last 50 runs from `<state>/runs.jsonl`. The
`cache` field carries counters of cache integrations (e.g. sccache hits).
`offshore status` estimates today's cost (since local midnight) per
machine size as `price_per_hour` times the uptime the orchestrator observed
(starting or running). Machines first seen up count from their creation
time; restarts and removals that happened while the daemon or control plane
was not running are not visible, and provider billing granularity is
ignored.

`offshore doctor [--json]` checks and explains: `offshore` in PATH
and the shims first in PATH for their tools, the client configuration, the
control plane (reachable, certificate fingerprint, API key accepted, clock
skew from its `Date` header) or the embedded pool configuration, the daemon,
the machines of the pool, the repository configuration, every secret (it is
resolved, its value is never printed), the environment image (where it
exists, whether it will be built, the last background build) and FUSE
(`/dev/fuse`, `fusermount3`; required only with `[outputs] mode = "fuse"`).
In embedded mode with a local-docker provider the `cache medium` check
inspects the file system of Docker's data root (`docker info`
`DockerRootDir`, where the machines' cache volumes live) and warns when it
is not reflink capable (not xfs/btrfs, or the FICLONE probe fails when the
directory is writable) or ext4 mounted with `noauto_da_alloc`
([caches-and-storage.md](caches-and-storage.md#build-slots-on-anyfs-commit-writeback-lazy-restore)).
The exit status is 1 when a check failed.

**Cache medium of cloud machines.** Cloud workers require xfs with reflink
for their cache directory. A worker that finds something else keeps
serving degraded and says so: the log line `cache medium breaks the
machine policy`, `cache_fs` in its status (`offshore status --json`
machine status: `type`, `reflink`, `ok`, `problems`) and
`offshore_worker_cache_fs_ok{type,policy} 0`. Alert on that metric; the
usual cause is a size without instance-store disk or volume on an image
whose root file system is ext4. Fix it with `medium = "volume"` or an
instance type with local NVMe (the supervisor formats both as xfs).

## Machine loss and network interruptions

**Source link.** The daemon keeps one connection per machine that serves
the source files the machine's file system faults in. When it breaks (a
network interruption: `timeout: no recent network activity`), the daemon
redials with backoff for up to two minutes and attaches the roots again; a
failed attach of a new root no longer closes the connection that running
sessions use. On the worker, a source read whose client is unreachable,
or whose transfer makes no progress for 20 s, blocks and retries until the
client is back, for at most 60 s from the first failure; only then does
the build see an I/O error (`EIO`). The worker logs
`source client unreachable, retrying` and `source fetch failed`.

**Retry of the command.** If a session ends without an exit status because
the connection to its machine broke (the machine stopped, was destroyed or
the network failed), the proxy runs the command once more and prints one
line saying so (`the connection to machine … broke` when the machine
answers again, `machine … was lost` otherwise); the run log records
`lost_machine`. A session the machine killed is retried the same way:
an OOM kill (`killed by OOM on shared machine …, retrying`; its usage is
reported first, so the estimator reserves more memory for the key on the
next placement) or the worker stopping under it (`worker_shutdown`). The
control plane writes a `task_killed` event next to the `task_end` of such
sessions. Write-back stays exactly-once: local files only change in the
write-back phase of the attempt that reported an exit status and is not
retried, so a retried attempt has applied nothing. There is no retry when
the write-back had begun, standard input had been forwarded (it cannot be
replayed), the user signalled the command, the session had not started yet
(that falls back as before), the worker reported an error, after a second
failure, or with `OFFSHORE_NO_RETRY=1`. Output the first attempt already
printed stays on the terminal.

**Control plane restarts.** Requests the control plane never received
(connection refused, no route, a proxy answering 502/503) are retried
with backoff for up to 60 s, with one line `control plane unreachable …,
waiting`; then the run falls back or fails as before.

**Machine stops.** A stop or destroy first saves the machine's changed
build slots, which takes minutes. If the machine is undrained or a session
is placed on it meanwhile, the stop is cancelled (control plane log: `stop
of machine cancelled`); once committed the machine is not undrained.

## Metrics

All components export Prometheus text format (no extra
dependencies; `internal/metrics`):

* Control plane: `GET /metrics` on the API port (API key required) and,
  with `--metrics-listen 127.0.0.1:9090` (`$OFFSHORE_METRICS_LISTEN`),
  without authentication on a separate plain HTTP listener for a local
  scraper. Pool metrics: `offshore_machines{provider,size,state}`,
  `offshore_queue_length`, `offshore_acquire_wait_seconds{result}`,
  `offshore_placements_total{kind}` (assign, queue, reject, timeout),
  `offshore_pool_actions_total{action}`,
  `offshore_provider_errors_total{provider,op}`,
  `offshore_image_builds_total{provider,result}`,
  `offshore_image_build_seconds{provider}`,
  `offshore_reconcile_duration_seconds{result}`,
  `offshore_cost_per_hour{provider}`, `offshore_environments_recent`,
  `offshore_http_requests_total{path,result}`,
  `offshore_http_request_duration_seconds{path}`.
* Machines report their metrics in the status stream the orchestrator
  already probes (scope `probe`, no extra port), and the orchestrator
  republishes them with `machine`, `provider` and `size` labels:
  `offshore_worker_sessions_running`,
  `offshore_worker_session_duration_seconds`,
  `offshore_worker_session_exits_total{code}`,
  `offshore_worker_oom_kills_total`,
  `offshore_worker_sessions_rejected_total{reason}`,
  `offshore_worker_pressure{resource}`,
  `offshore_worker_reserved{resource}`,
  `offshore_worker_memory_used_bytes`,
  `offshore_worker_compiler_cache_total{counter}` (sccache counters),
  `offshore_worker_transfer_bytes_total{direction}` (fetch, writeback,
  output), `offshore_worker_ready`, `_draining`, `_warm_repos`,
  `offshore_worker_cache_fs_ok{type,policy}` (0: the cache medium breaks
  the machine's file system policy), `offshore_worker_cache_fs_reflink`,
  and the
  cache file system in virtual memory terms
  ([caches-and-storage.md](caches-and-storage.md#anyfs-one-storage-core)):
  `offshore_cache_resident_bytes`, `_dirty_bytes`, `_working_set_bytes`,
  `_fault_rate`, `_reclaim_bytes`, `_thrashing`. Machine counters restart
  with the worker.
* `offshore metrics` prints the daemon's metrics
  (`offshore_daemon_requests_total{op,result}`,
  `offshore_daemon_prepare_seconds{result}`,
  `offshore_daemon_runs_total{tool,mode}`, `offshore_daemon_roots`,
  `offshore_daemon_machine_connections`) with the embedded
  orchestrator's (embedded mode) or the control plane's (remote mode).

## Logs

`OFFSHORE_LOG_FORMAT=json` (default `text`) and
`OFFSHORE_LOG_LEVEL=debug|info|warn|error` apply to the daemon
(`<state>/daemon.log`), the control plane and workers; the
orchestrator passes both to the machines it creates. The control plane logs
every API request with a `request_id` (the client's `X-Request-Id` when
well formed, returned in the response header), and acquisitions with
lease, machine and placement reason.
The daemon log is `<state>/daemon.log`, background prewarms log to
`<state>/prewarm.log` and environment image builds to `env-build.log`
(`<state>` is the daemon state directory, `~/.local/state/offshore` by
default). `OFFSHORE_DEBUG=1` is a shortcut for the debug level.

## Tracing

OpenTelemetry tracing is off by default. The standard
variables enable it in every component (`OTEL_TRACES_EXPORTER=otlp|console|none`,
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER[_ARG]`,
`OTEL_SDK_DISABLED`), as does a `[telemetry]` section in the client and the
pool configuration (the environment wins):

```toml
[telemetry]
endpoint = "https://otel.example.com:4318"  # OTLP/HTTP base URL
headers = { "x-api-key" = "env:OTEL_KEY" }  # references only (env:, file:, cmd:)
sampling = 0.25                             # new traces; downstream follows the parent
machine_endpoint = "http://10.0.0.5:4318"   # pool file: the collector as seen by machines
```

Spans are exported as OTLP/HTTP with JSON encoding (accepted by the
OpenTelemetry Collector and OTLP backends on `/v1/traces`; OTLP/gRPC is not
supported, which keeps gRPC and protobuf out of the static binary). Default
service names: `offshore-proxy`, `-daemon`, `-controlplane`, `-worker`,
`-supervisor`. The orchestrator forwards the exporter settings to the machines it
creates (headers as secrets; `machine_endpoint` or
`$OFFSHORE_OTEL_MACHINE_ENDPOINT` overrides the endpoint), workers do
not pass `OTEL_*` on to sessions, and `console` writes to stderr or to
`$OFFSHORE_OTEL_CONSOLE_FILE`. One tool run is one trace, propagated as
W3C `traceparent` (daemon RPC field, control plane HTTP header, optional
`traceparent` field of the QUIC `Hello` and `Exec` frames):

```
offshore.run (proxy: tool, subcommand, flag names, repo id, decision, reason, exit code)
├─ proxy.prepare
│  └─ daemon.prepare (repo id, env hash, usage key, estimate, lease, machine)
│     ├─ daemon.env_resolve, daemon.image_ensure, daemon.manifest_refresh, daemon.estimate
│     ├─ [remote] controlplane /v1/acquire (client) > controlplane /v1/acquire (server)
│     │  └─ pool.acquire (queue wait, placement, reason, machine, provider, size, price, reservation)
│     │       ~ pool.create / pool.start (linked root spans: provider, size, price, duration, ready event)
│     └─ daemon.attach (machine, attempt)
├─ proxy.connect (session, worker wait)
├─ worker.exec (machine: lease, reservation, slot, warm, peak memory, CPU seconds, OOM, exit code, sccache counters)
│  ├─ worker.process, worker.writeback, worker.outputs
├─ proxy.exec (exit code, usage), proxy.writeback, proxy.outputs
└─ daemon.release > controlplane /v1/release
orchestrator.reconcile > pool.stop / pool.destroy (background, provider latency)
```

Only the subcommand and the names of flags of a tool's arguments are
recorded, never their values, positional arguments, environment or tokens.

## Agents, worktrees and labels

Every session carries labels: `worktree` (the repository root, i.e. the
worktree), `repo` (name of the main repository, shared by its worktrees),
`branch`, `user` (client config `user`) and `agent`. They are computed by the
proxy (git metadata is read from `.git` files, git is not run), travel with
the session to the daemon, the orchestrator and the worker, and are recorded
in the event log (`labels` of `task_placed`/`task_end`) and the run log.
`offshore why`, `status`, `top` and `report` show them.

`agent` is `OFFSHORE_AGENT` if set, otherwise detected from the first
present variable: `CODEX_THREAD_ID` (codex, the thread id also becomes
`agent_session`), `CODEX_SANDBOX`, `CODEX_MANAGED_BY_NPM` (codex),
`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT` (claude-code), `CURSOR_AGENT`
(cursor), `GEMINI_CLI` (gemini-cli), `OPENCODE` (opencode), `AIDER_MODEL`
(aider), `AMP_THREAD_ID` (amp, also `agent_session`). Only these names and
ids are read, never other environment values. `OFFSHORE_LABELS="task=WP-7,team=infra"`
adds free labels. Keys are `[a-z0-9_.-]{1,32}`, values at most 200
characters, at most 16 labels; workers clean labels again on receipt.
Labels never influence placement.

`offshore report` groups cost (attributed by reserved share, as per
repository), session-hours, CPU-hours, requests and failures, queue wait
p50/p95/max and contention (share of wall time stalled on CPU/memory) by
agent and by worktree; `--by agent|worktree|branch|repo|user` (comma
separated) chooses the dimensions. Events from before labels fall back to
the repository id; sessions without an agent count as `(human)`.

## Prewarm

`offshore prewarm` starts the remote environment of the
current repository in the background (log `<state>/prewarm.log`): image
ensure, machine lease and start, source sync and the build slot, then an
optional warm-up command from the repository configuration:

```toml
[prewarm]
run = ["cargo", "check", "--workspace", "--tests"]
```

`--wait` runs in the foreground, `--force` skips the warm check. A prewarm
is skipped when a running machine of the environment already has a warm
build slot of the repository and (with `run`) the warm-up succeeded for the
same commit, environment and command within 12 hours. Only one prewarm per
repository runs at a time. Prewarm sessions are labelled `purpose=prewarm`
(shown in `why` and `report`), never fall back to local execution and are
not counted as user runs in the daemon metrics. `offshore hooks install`
installs a git `post-checkout` hook into the repository's hooks directory
(shared by all worktrees, honours `core.hooksPath`) that calls
`offshore prewarm --quiet` after branch checkouts, which includes `git
worktree add`. An existing hook is chained (renamed to
`post-checkout.offshore-chain`, runs first, its exit status is kept);
install is idempotent and `offshore hooks uninstall` restores the
previous hook.

## Top

`offshore top` is a full-screen view in the style of zenith that uses the
whole terminal (the height is split between the graphs and the tables on
every resize; tiny terminals keep one row each, tables scroll):

* graphs of the active (running/starting) machines: busy cores and session
  memory, seeded from the `machine_sample` events and extended by the 2 s
  status polls (kept in memory for 3 h);
* Machines: aligned columns MACHINE, PROVIDER/SIZE, STATE, CPU, MEM, DISK
  (bar plus used/total), SESS, $/H and, when the worker reports its cache
  file system (`Status.VM`), CACHE (resident/capacity), DIRTY, WS (working
  set) and FAULTS (per second, `THRASH`); optional columns are dropped
  first on narrow terminals. The panel title shows `$X/h now · $Y today`.
  Default order: busy, idle (shortest idle first), booting, draining,
  stopped, failed, then age. A machine that is up at the provider but whose
  worker does not answer yet or is still setting up shows `booting` with
  the time since its start and no capacity bars; a worker that stopped
  admitting sessions shows `draining (disk full)`;
* Storage: the disk of each running machine broken down by the worker
  (`Status.Disk.Items`): a header row (used/total, budget), then build
  slots (worktree and branch of their last session, `*` while leased) next
  to their repository's local page tier, materialized files, source
  contents, machine caches and global caches by name, and the rest of the
  file system (images, system); columns KIND, NAME, SIZE, RESIDENT,
  STORED (the repository head in the store), DIRTY, LAST USED, COMMITTED
  and LAG (how far the store head is behind the slot's last commit). The
  title sums the store bytes per repository and the page-in/writeback
  rates. Enter opens a storage detail: all fields, the committed head by
  access class (immutable, write-once, churn, ephemeral) and its largest
  directories (measured every 5 minutes), the working set and the
  repository's commits, restores, writebacks, page-ins, prunes and
  reclaims seen by top;
* Sessions: running sessions, then queued requests, then recently
  finished sessions (exit code, duration, average cores, peak memory) in
  one table: MACHINE, SESSION, STATE, USER@HOST, WORKTREE, BRANCH, ELAPSED,
  CPU and MEM (bar plus used/reserved), COMMAND.

Keys: ↑/↓ PgUp/PgDn select, Tab cycles through the session, machine and
storage tables, Enter opens a detail view (Esc/q back, ↑/↓ the next row), `r`
reverses, `+`/`-` zoom the graph window (1m..3h), `q` quits. Sessions sort
by `e c m w n u` (elapsed, CPU, memory, worktree, machine, user@host),
machines by `s c m d p a n` (state, CPU, memory, disk, price, age, name);
pressing the key again reverses. The mouse selects a row; clicking the
selected row opens it. The machine detail shows all fields, the lifecycle
history, CPU/memory/pressure/disk graphs of the machine, its build slots,
running and recent sessions and its cost today; the session detail shows
command, client, worktree, branch, repository, labels, machine, placement
(reason, wait), live CPU/memory graphs and, for finished sessions, exit
code, usage, build slot and stall figures.
`offshore top --once [--width N] [--window 10m]` prints one plain text
frame without colors, for agents and scripts. Busy cores per session come
from the session's cgroup CPU time between polls (`Status.Sessions[]
cpu_seconds`, `used_memory`).

## Event log and reports

Independent of tracing, the orchestrator writes
an analysis event log (`events-YYYY-MM-DD.jsonl` in its state directory,
`[telemetry] events_retention_days`, default 30) of task and machine events,
including the cache file system's `page_fault` (Pages), `page_in` and
`reclaim` (Bytes) per machine and sample interval, `writeback` per
committed slot head and `thrash` when a machine starts thrashing.
`offshore report [--since 7d] [--by agent,worktree] [--json]` shows cost per day, repository
and size, per agent/worktree/branch/repo/user, utilisation and overprovisioning, queue wait p50/p95, warm vs.
cold caches and estimator accuracy, machine samples per size and, from
samples with a cache file system (`machine_sample.vm`), resident/capacity,
dirty and working set p95, fault rate and the share of samples thrashing; `offshore sim export --since 7d -o
trace.json` converts the real runs into a simulator trace (both work
embedded and remote, via the control plane's `POST /v1/events`, API key
protected). See [scheduling.md](scheduling.md#calibrating-the-simulator-from-real-runs).

### Explainable scheduling and traceable reservations

Every placement and every scale-out start/create carries a `decision`:

```json
{"policy": "options", "cost_optimization": 0.5, "time_value_per_hour": 0.4, "chosen": 0,
 "options": [
  {"kind": "assign", "machine": "m-b", "provider": "aws", "size": "c7a.4xlarge", "slot": "restore",
   "reason": "restore snapshot from aws", "prep_seconds": 42, "page_in_seconds": 0, "run_seconds": 342,
   "time_seconds": 342, "cost": 0.031, "score": 0.069, "chosen": true},
  {"kind": "wait_machine", "machine": "m-a", "slot": "warm", "wait_seconds": 480, "run_seconds": 300,
   "time_seconds": 780, "cost": 0.027, "score": 0.114},
  {"kind": "assign", "machine": "m-b", "slot": "cold", "time_seconds": 600, "cost": 0.05, "score": 0.117},
  {"kind": "wait_machine", "machine": "m-c", "wait_seconds": 900, "rejected": "wait exceeds start/create ETA + margin (2m0s)"}]}
```

- `kind`: `assign` (run now), `wait_machine` (wait for a busy or starting machine), `wait_capacity`
  (scale-out: let the queued tasks wait for what is there or on its way), `start`, `create`, `reject`.
- `slot`: `warm`, `cold`, `restore` (snapshot from the store), `move` (save the warm slot elsewhere and
  restore it here: the "move cache" option next to waiting for the warm machine), `prune+…`, `resize+…`.
- Times in seconds: `wait`, `latency` (start/boot), `prep` (restore/move/prune/resize), `page_in`
  (predicted thrash of a working set beyond the local tier), `run` (prep + run), `time` (time to result;
  for scale-out summed over the queued tasks, `tasks` of which the option takes). `cost` is money.
- `score` is what the policy ranked by: option policy `cost + time_value_per_hour × hours`
  (`time_value_per_hour` -1: only time counts); `costmodel` the normalised score. Lower wins; the chosen
  option comes first, rejected options (not scored, with `rejected`) last.
- `task_placed.decision` is the placement (also on timeouts/rejects: the last decision), scale-out
  decisions are on `machine_create`/`machine_start`.

`task_placed.estimate` traces the reservation: `key` (estimator key), `need`, `fallback` (repository
resources), `cpu_source`/`memory_source` (`repo_config`, `learned_max` (before `min_samples` runs),
`learned`, `oom_backoff`), `samples`, `statistic` (e.g. `memory p90 of 20 peaks x 1.25`), and the
predicted `peak_memory`/`cores`.

While a request is queued, `task_waiting` is written every 15 s: `reason`, `wait_ms`, and `waiting`
`{for: wait_machine|wait_capacity|none, machine, provider, size, creating, eta_seconds}`; `offshore top`
shows the same in the queue (`QueuedTask.waiting`).

`task_end.predicted` compares the placement's prediction with the run: `run_seconds`/`expected_seconds`
(median/pessimistic), `cores`, `peak_memory` vs. `actual_*` and the ratios `run_ratio`, `cores_ratio`,
`memory_ratio` (actual / predicted).

### OOM and kill attribution

The worker reads the session cgroup's `memory.events` and the machine's `/proc/vmstat oom_kill`. In
`task_end.usage`: `oom`, `signal` (also the signal a wrapper reports with exit 129–192, e.g. a shell
exiting 137 for a SIGKILLed child), `killer` (`cgroup_oom`, `machine_oom`, `client_disconnect`,
`client_signal`, `worker_shutdown`, `signal`, `sigkill_unknown`), `oom_kills`, `memory_max_events`,
`memory_high_events`. Exit 137 with `killer: sigkill_unknown` means something outside offshore
(earlyoom, systemd-oomd, a user) killed it.

### Provider errors

Failed creates/starts are classified (`error_class` on `provider_down` and on the `machine_destroy`
with `reason: create_failed`): `capacity` and `quota` cool the provider down (2 min doubling up to
30 min, `reason: cooldown`); `account_limit` (e.g. OCI `LimitExceeded … upgrade`, free tier) and `auth`
**disable** it (`reason: disabled`, logged as `PROVIDER DISABLED` once) until the pool configuration is
reloaded or `offshore provider reset NAME`; `transient` and `other` use the per-environment create
backoff. `offshore status` and `offshore doctor` show disabled and cooling providers.

### Per-session time series

The worker samples each session cgroup every 5 s (busy cores, `memory.current`, `io.stat` read/write
bytes, PSI some avg10 of cpu/memory/io, `memory.high` events) and reports the last minute in its
status (`proto.Session.samples`). The orchestrator logs every new sample once as `session_sample`
(`task`, `machine`, `provider`, `repo`, `key`, `labels`, `sample {time, cpu, memory, io_read_bytes,
io_write_bytes, psi_cpu, psi_memory, psi_io, memory_high}`), retained like all events, keeps the last
hour per session in memory (10 min after it ended) and, with `[telemetry]` OTLP configured, exports
the gauges `offshore.session.cpu|memory|io.read|io.write|pressure.cpu|pressure.memory|pressure.io`
(attributes `offshore.lease`, `offshore.machine`, `offshore.repo_id`, `offshore.usage_key`) to
`…/v1/metrics`. Read them with `POST /v1/sessions/samples {"lease": "...", "since": "..."}` (control
plane, API key), the daemon op `session-samples` (`lease`, `since`) or
`Orchestrator.SessionSamples(lease, since)`; older sessions fall back to the event log (last day).
Provider health: `POST /v1/providers`, `POST /v1/providers/reset {"name": "oci"}`, daemon ops
`providers` and `provider-reset`, `offshore provider`, `offshore status --json` (`providers`).

### Robust raw data

Each event is one line written with a single `write(2)` on an `O_APPEND` file; a short or failed write
(ENOSPC) is truncated away, so files never contain partial lines. Dropped events are counted in
`offshore_events_dropped_total` and the first drop is logged; readers skip unparsable lines and count
them in `offshore_events_skipped_lines_total`.

## 24 h guarantees and cleanup

| What | Bound | Enforced by |
| --- | --- | --- |
| machines | `max_lifetime` (default 24h) | worker drains (saving slots to the store) and exits at expiry; orchestrator destroys; AWS: instance shuts down (terminate on shutdown), reaper Lambda terminates 5 min after `expires-at` every 10 min; OCI: the supervisor terminates its instance through the API; static: the supervisor removes the machine |
| build slots | slot cache `ttl` (default 24h), capped by `max_lifetime` | worker garbage collection and pruning; die with the machine's cache volume |
| global caches (sccache, ccache, go), snapshots | 24h | never served when older; aws-ec2 bucket lifecycle on `cache/` deletes after 1 day; snapshot chunks older than 12 h are uploaded again |
| machine caches | `ttl` of `scope = "machine"` caches (default 7 days) | volume expiry label, orchestrator reaps volumes |
| machine secrets (AWS SSM) | until the supervisor reads them | supervisor deletes after reading; reaper deletes parameters under `/offshore/<stack>/machines/` without a live machine (after 15 min) |
| worker binaries (S3 `bin/`) | 30 days | bucket lifecycle |
| environment images (ECR) | 30 days | repository lifecycle |

`offshore down` destroys all machines and caches of the pool;
`offshore reconcile` reloads the pool configuration and applies the
policy immediately.
