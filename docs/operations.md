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
The exit status is 1 when a check failed.

## Machine loss

If a session ends without an exit status because the
connection to its machine broke (the machine stopped, was destroyed or became
unreachable) and the machine does not answer a new connection, the proxy runs
the command once more on another machine and prints one line saying so; the
run log records `lost_machine`. Write-back stays exactly-once: local files
only change in the write-back phase of the attempt that reported an exit
status, so the lost attempt has applied nothing. There is no retry when the
write-back had begun, standard input had been forwarded (it cannot be
replayed), the user signalled the command, the session had not started yet
(that falls back as before), the worker reported an error, the machine still
answers, after a second loss, or with `OFFSHORE_NO_RETRY=1`. Output the
first attempt already printed stays on the terminal.

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
  output), `offshore_worker_ready`, `_draining`, `_warm_repos`. Machine
  counters restart with the worker.
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

`offshore top` is a full-screen view in the style of zenith:
history graphs of the pool (busy cores, session memory, sessions plus queue,
cost per hour; seeded from the `machine_sample` events and extended by the
2 s status polls, kept in memory for 3 h), one line per machine (busy-core
bar and history, memory bar, PSI, sessions, price), a sortable session table
(machine, agent, worktree, branch, elapsed, busy/reserved cores,
used/reserved memory, command) and the queue. Keys: ↑/↓ PgUp/PgDn select,
`e c m a w n` sort by elapsed, CPU, memory, agent, worktree, machine (again:
reverse), `r` reverse, `+`/`-` zoom the graph window (1m..3h), `q` quit.
`offshore top --once [--width N] [--window 10m]` prints one plain text
frame without colors, for agents and scripts. Busy cores per session come
from the session's cgroup CPU time between polls (`Status.Sessions[]
cpu_seconds`, `used_memory`).

## Event log and reports

Independent of tracing, the orchestrator writes
an analysis event log (`events-YYYY-MM-DD.jsonl` in its state directory,
`[telemetry] events_retention_days`, default 30) of task and machine events.
`offshore report [--since 7d] [--by agent,worktree] [--json]` shows cost per day, repository
and size, per agent/worktree/branch/repo/user, utilisation and overprovisioning, queue wait p50/p95, warm vs.
cold caches and estimator accuracy; `offshore sim export --since 7d -o
trace.json` converts the real runs into a simulator trace (both work
embedded and remote, via the control plane's `POST /v1/events`, API key
protected). See [scheduling.md](scheduling.md#calibrating-the-simulator-from-real-runs).

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
