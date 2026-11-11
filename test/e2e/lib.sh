# Shared setup and helpers of the e2e parts (sourced by parts/*.sh).
#
# Every part has its own owner, state directory, daemon (and control plane
# in remote mode), test clock and free local ports, so parts can run
# concurrently (run.sh E2E_PARALLEL=1). A part sets PART and sources this
# file; the checks follow.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
BIN="${E2E_BIN:-$REPO/bin/offshore}"
DOCKER="${E2E_DOCKER:-docker}"
PART="${PART:-e2e}"
WORK="$(mktemp -d "/tmp/offshore-e2e-$PART.XXXXXX")"
OWNER="e2e-$(basename "$WORK" | tr -dc 'a-zA-Z0-9' | tr 'A-Z' 'a-z')"
PROJ="$WORK/hello"

export OFFSHORE_STATE_DIR="$WORK/state"
export OFFSHORE_CONFIG="$WORK/config.toml"
POOLCFG="$OFFSHORE_CONFIG"   # pool and providers; the control plane's in remote mode
REMOTE="${E2E_REMOTE:-}"
CPSTATE="$WORK/cp-state"
CPPID=""
export OFFSHORE_SHIM_DIR="$WORK/shims"
export OFFSHORE_REQUIRE_REMOTE=1
export OFFSHORE_LOG_FORMAT=json   # daemon, control plane and (forwarded) machines
# Test clock (internal/clock): idle stop and lifetime expiry follow
# `offshore debug clock advance` instead of real minutes. Fast
# reconciles act on it (and on everything else) within a second.
export OFFSHORE_TEST_CLOCK="offset:$WORK/clock"
export OFFSHORE_RECONCILE_INTERVAL="${E2E_RECONCILE_INTERVAL:-1s}"
unset OFFSHORE_DISABLE OFFSHORE_ACTIVE

PASS=0
FAIL=0
T0=$SECONDS
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
step() { printf '\n== %s  [%s +%ds]\n' "$*" "$PART" $((SECONDS - T0)); LAST=$SECONDS; }
# check NAME CMD...: a check; slow ones (waits included) show their time.
LAST=$SECONDS
check() { local name="$1" rc=0 t; shift; "$@" || rc=1; t=$((SECONDS - LAST)); [ $t -ge 5 ] && name="$name  (${t}s)"; if [ $rc = 0 ]; then ok "$name"; else bad "$name"; fi; LAST=$SECONDS; }
eq()   { [ "$1" = "$2" ] || { echo "     expected: $2"; echo "     got:      $1"; return 1; }; }
contains() { case "$1" in *"$2"*) return 0;; esac; echo "     '$2' not in output:"; echo "$1" | sed 's/^/       /' | tail -20; return 1; }
cs()   { "$BIN" "$@"; }
# sccache_hits extracts N from the verbose line "offshore: sccache: N hits / M misses ...".
sccache_hits() { local n; n="$(echo "$1" | sed -n 's/.*sccache: \([0-9]*\) hits.*/\1/p' | tail -1)"; echo "${n:-0}"; [ -n "$n" ] || echo "$1" | tail -5 | sed 's/^/       /' >&2; }
run()  { (cd "$PROJ" && "$BIN" run "$@"); }
docker_() { $DOCKER "$@"; }
# freeport prints a currently free local TCP port. Fixed ports collide with
# concurrent e2e runs on the same host.
freeport() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
# poll SECONDS CMD...: retry CMD (quietly) until it succeeds or the time is
# up, then run it once more with its output (the check's diagnostics).
poll() { local end=$((SECONDS + $1)); shift; while [ $SECONDS -lt $end ]; do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done; "$@"; }
# no_sessions: no machine runs a session (sessions column of cs status).
no_sessions() { cs status | awk '/^[0-9a-f]+ / && $(NF-2) > 0 {busy=1} END {exit busy}'; }
# machines: ids of this part's machine containers.
machines() { docker_ ps -q --filter "label=offshore.pool=$OWNER" --filter label=offshore.kind=machine; }
# metric TEXT NAME LABELSUBSTR: value of the first sample of NAME whose labels contain LABELSUBSTR.
metric() { local v; v="$(echo "$1" | awk -v n="$2" -v l="$3" 'index($0, n"{")==1 && index($0, l) {print $NF; exit}')"; echo "${v:-0}"; }
# metric_ge NAME LABELSUBSTR MIN: the current value (cs metrics) is at least MIN.
metric_ge() { local v; v="$(metric "$(cs metrics)" "$1" "$2")"; [ "${v%.*}" -ge "$3" ] || { echo "     $1{$2} = $v"; return 1; }; }
# advance DURATION: move the test clock of the orchestrator forward.
advance() { cs debug clock advance "$1" >/dev/null; }
# ready: the environment image exists (run.sh starts the other parallel
# parts only then, so that it is built once).
ready() { [ -n "${E2E_READY:-}" ] && touch "$E2E_READY"; return 0; }
# wait_release NAME prints a bash snippet for sessions that waits (up to
# 120s) until `release NAME` from the test: a file NAME appears in /etc of
# the machine's web service, which sessions reach on 127.0.0.1:18080.
# Sessions so last exactly as long as a check needs them.
wait_release() { printf 'for i in $(seq 1200); do (exec 3<>/dev/tcp/127.0.0.1/18080 && printf "GET /%s HTTP/1.0\\r\\n\\r\\n" >&3 && grep -q "200 OK" <&3) 2>/dev/null && break; sleep 0.1; done' "$1"; }
services() { docker_ ps -q --filter "label=offshore.pool=$OWNER" --filter label=offshore.kind=service --filter status=running; }
# services_up: every running machine has its (running) web service.
services_up() { [ "$(services | wc -l)" -ge "$(machines | wc -l)" ]; }
release() { local svc; poll 60 services_up >/dev/null; for svc in $(services); do docker_ exec "$svc" touch "/etc/$1"; done; }
http_service() { run sh -c 'for i in $(seq 50); do bash -c "exec 3<>/dev/tcp/127.0.0.1/18080 && printf \"GET /hostname HTTP/1.0\r\n\r\n\" >&3 && cat <&3" 2>/dev/null && exit 0; sleep 0.2; done; exit 1'; }

cleanup() {
  step "cleanup"
  # timed STEP CMD...: runs a teardown step and reports slow ones.
  timed() { local t=$SECONDS; "$@" >/dev/null 2>&1; [ $((SECONDS - t)) -ge 5 ] && echo "     ($* took $((SECONDS - t))s)"; return 0; }
  timed cs outputs unmount
  timed cs down
  timed cs daemon stop
  timed stop_cp
  # The pool's directory store (layers and pages) outlives "down" on
  # purpose; the test pool is gone, so is its store.
  timed docker_ volume rm -f "cs-store-$OWNER"
  local left
  left="$(docker_ ps -aq --filter "label=offshore.pool=$OWNER" | wc -l)"
  left=$((left + $(docker_ volume ls -q --filter "label=offshore.pool=$OWNER" | wc -l)))
  if [ "$left" = 0 ]; then ok "down removed all machines and volumes"; else bad "down left $left resources"; fi
  local t=$SECONDS
  if [ "${E2E_KEEP:-}" = "" ]; then chmod -R u+w "$WORK" 2>/dev/null; rm -r "$WORK"; else echo "kept $WORK"; fi
  [ $((SECONDS - t)) -ge 5 ] && echo "     (removing $WORK took $((SECONDS - t))s)"
  printf '\n%s: %d passed, %d failed (%ds)\n' "$PART" "$PASS" "$FAIL" $((SECONDS - T0))
  [ -n "${E2E_RESULT:-}" ] && echo "$PASS $FAIL $((SECONDS - T0))" > "$E2E_RESULT"
  [ "$FAIL" = 0 ]
}
trap 'cleanup; exit $?' EXIT

[ -x "$BIN" ] || { echo "missing $BIN (run make build)"; exit 2; }
docker_ info >/dev/null 2>&1 || { echo "docker not usable via: $DOCKER"; exit 2; }

# Tracing: every component exports spans to a local OTLP/HTTP stub
# (test/e2e/otlp_sink.py); machines reach it through the Docker bridge.
OTLP_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1])')"
OTLP_OUT="$WORK/otlp.jsonl"
python3 "$HERE/otlp_sink.py" "$OTLP_PORT" "$OTLP_OUT" $$ >/dev/null 2>&1 &
export OTEL_TRACES_EXPORTER=otlp OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:$OTLP_PORT"
export OFFSHORE_OTEL_MACHINE_ENDPOINT="http://$(docker_ network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}'):$OTLP_PORT"

cp -r "$HERE/fixture/hello" "$PROJ"
echo file-secret > "$PROJ/.secret"
RUSTV="${E2E_RUSTV:-$(docker_ run --rm rust:1-slim-bookworm rustc --version | awk '{print $2}')}"
printf '[toolchain]\nchannel = "%s"\nprofile = "minimal"\n' "$RUSTV" > "$PROJ/rust-toolchain.toml"
# Local port of the [tool.bash] forward, free on this host.
FWD_SVC="$(freeport)"
sed -i "s/\"18092:18080\"/\"$FWD_SVC:18080\"/" "$PROJ/.offshore.toml"

# write_config writes the pool configuration: the client file in embedded
# mode, the control plane's own file in remote mode.
write_config() { # lifetime idle min max
  cat > "$POOLCFG" <<EOF
[pool]
id = "$OWNER"
max_lifetime = "$1"
idle_stop = "$2"
min_machines = $3
max_machines = $4
queue_timeout = "3m"

[scheduler]
estimator = "static"
# local-docker machines share the host's file system, mostly filled by
# unrelated data: the default budget (90% of the disk) would prune the
# build slots cold after every session on a full host.
disk_budget = "100%"

[[machine.setup]]
run = "echo provisioned-by-pool > /cache/pool-provision"

[providers.local]
type = "local-docker"
command = [$(printf '"%s",' $DOCKER | sed 's/,$//')]

[[providers.local.size]]
name = "default"
cpu = 4
memory = "4G"
price_per_hour = 0.5
EOF
}
stop_cp() { if [ -n "$CPPID" ]; then kill "$CPPID" 2>/dev/null; wait "$CPPID" 2>/dev/null; CPPID=""; fi; }
start_cp() {
  OFFSHORE_CONFIG=/nonexistent OFFSHORE_STATE_DIR="$CPSTATE" "$BIN" controlplane --config "$POOLCFG" --listen "127.0.0.1:$CPPORT" --metrics-listen "127.0.0.1:$MPORT" >>"$WORK/cp.log" 2>&1 &
  CPPID=$!
  for _ in $(seq 300); do curl -skf "https://127.0.0.1:$CPPORT/healthz" >/dev/null && return 0; sleep 0.1; done
  echo "control plane did not start:"; cat "$WORK/cp.log"; return 1
}
# restart_daemon applies a changed pool configuration.
restart_daemon() { cs daemon stop >/dev/null; if [ -n "$REMOTE" ]; then stop_cp; start_cp; fi; }

if [ -n "$REMOTE" ]; then
  POOLCFG="$WORK/cp-config.toml"
  CPPORT="$(freeport)"
  MPORT="$(freeport)"
  write_config 24h 10m 0 2
  start_cp || exit 1
  FP="$(sed -n 's/.*"Fingerprint":"\([0-9a-f]*\)".*/\1/p' "$CPSTATE/tls.json")"
  cat > "$OFFSHORE_CONFIG" <<EOF
[control_plane]
url = "https://127.0.0.1:$CPPORT"
fingerprint = "$FP"
api_key = "file:$CPSTATE/api-key"
EOF
fi
write_config 24h 10m 0 2
