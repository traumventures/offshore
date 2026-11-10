#!/usr/bin/env bash
# End-to-end test of cloudsourced with the local-docker backend.
#
# Everything is real: environment image build, machine provisioning as
# Docker containers, FUSE projection, sessions, write back, outputs,
# scheduling, scale out, idle stop, lifetime expiry and cleanup. The test
# uses its own owner, state directory and daemon, so it does not touch an
# existing setup.
#
# Requirements: docker (set E2E_DOCKER="sudo -n docker" if your user is not
# in the docker group), /dev/fuse, a static build at bin/cloudsourced.
#
# E2E_REMOTE=1 runs the same test against a shared control plane
# ("cloudsourced controlplane", self-signed TLS, API key) instead of the
# orchestrator embedded in the daemon.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
BIN="${E2E_BIN:-$REPO/bin/cloudsourced}"
DOCKER="${E2E_DOCKER:-docker}"
WORK="$(mktemp -d /tmp/cloudsourced-e2e.XXXXXX)"
OWNER="e2e-$(basename "$WORK" | tr -dc 'a-zA-Z0-9' | tr 'A-Z' 'a-z')"
PROJ="$WORK/hello"

export CLOUDSOURCED_STATE_DIR="$WORK/state"
export CLOUDSOURCED_CONFIG="$WORK/config.toml"
FLEETCFG="$CLOUDSOURCED_CONFIG"   # fleet and providers; the control plane's in remote mode
REMOTE="${E2E_REMOTE:-}"
CPSTATE="$WORK/cp-state"
CPPID=""
export CLOUDSOURCED_SHIM_DIR="$WORK/shims"
export CLOUDSOURCED_REQUIRE_REMOTE=1
unset CLOUDSOURCED_DISABLE CLOUDSOURCED_ACTIVE

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
check() { local name="$1"; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }
eq()   { [ "$1" = "$2" ] || { echo "     expected: $2"; echo "     got:      $1"; return 1; }; }
contains() { case "$1" in *"$2"*) return 0;; esac; echo "     '$2' not in output:"; echo "$1" | sed 's/^/       /' | tail -20; return 1; }
cs()   { "$BIN" "$@"; }
# sccache_hits extracts N from the verbose line "cloudsourced: sccache: N hits / M misses ...".
sccache_hits() { local n; n="$(echo "$1" | sed -n 's/.*sccache: \([0-9]*\) hits.*/\1/p' | tail -1)"; echo "${n:-0}"; [ -n "$n" ] || echo "$1" | tail -5 | sed 's/^/       /' >&2; }
run()  { (cd "$PROJ" && "$BIN" run "$@"); }
docker_() { $DOCKER "$@"; }

cleanup() {
  step "cleanup"
  cs down >/dev/null 2>&1 || true
  cs daemon stop >/dev/null 2>&1 || true
  stop_cp
  local left
  left="$(docker_ ps -aq --filter "label=cloudsourced.owner=$OWNER" | wc -l)"
  left=$((left + $(docker_ volume ls -q --filter "label=cloudsourced.owner=$OWNER" | wc -l)))
  if [ "$left" = 0 ]; then ok "down removed all machines and volumes"; else bad "down left $left resources"; fi
  if [ "${E2E_KEEP:-}" = "" ]; then chmod -R u+w "$WORK" 2>/dev/null; rm -r "$WORK"; fi
  printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
  [ "$FAIL" = 0 ]
}
trap 'cleanup; exit $?' EXIT

[ -x "$BIN" ] || { echo "missing $BIN (run make build)"; exit 2; }
docker_ info >/dev/null 2>&1 || { echo "docker not usable via: $DOCKER"; exit 2; }

cp -r "$HERE/fixture/hello" "$PROJ"
echo file-secret > "$PROJ/.secret"
RUSTV="$(docker_ run --rm rust:1-slim-bookworm rustc --version | awk '{print $2}')"
printf '[toolchain]\nchannel = "%s"\nprofile = "minimal"\n' "$RUSTV" > "$PROJ/rust-toolchain.toml"

write_config() { # lifetime idle min max
  cat > "$FLEETCFG" <<EOF
[fleet]
owner = "$OWNER"
machine_cpu = 4
machine_memory = "4G"
max_lifetime = "$1"
idle_stop = "$2"
min_machines = $3
max_machines = $4
queue_timeout = "3m"
estimator = "static"

[[fleet.size]]
name = "default"
cpu = 4
memory = "4G"
price_per_hour = 0.5

[[fleet.provision]]
run = "echo provisioned-by-fleet > /cache/fleet-provision"

[docker]
command = [$(printf '"%s",' $DOCKER | sed 's/,$//')]
EOF
}
stop_cp() { if [ -n "$CPPID" ]; then kill "$CPPID" 2>/dev/null; wait "$CPPID" 2>/dev/null; CPPID=""; fi; }
start_cp() {
  CLOUDSOURCED_CONFIG="$FLEETCFG" CLOUDSOURCED_STATE_DIR="$CPSTATE" "$BIN" controlplane --listen "127.0.0.1:$CPPORT" >>"$WORK/cp.log" 2>&1 &
  CPPID=$!
  for _ in $(seq 100); do curl -skf "https://127.0.0.1:$CPPORT/healthz" >/dev/null && return 0; sleep 0.1; done
  echo "control plane did not start:"; cat "$WORK/cp.log"; return 1
}
# restart_daemon applies a changed fleet configuration.
restart_daemon() { cs daemon stop >/dev/null; if [ -n "$REMOTE" ]; then stop_cp; start_cp; fi; }

if [ -n "$REMOTE" ]; then
  FLEETCFG="$WORK/cp-config.toml"
  CPPORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  write_config 24h 10m 0 2
  start_cp || exit 1
  FP="$(sed -n 's/.*"Fingerprint":"\([0-9a-f]*\)".*/\1/p' "$CPSTATE/tls.json")"
  cat > "$CLOUDSOURCED_CONFIG" <<EOF
[orchestrator]
mode = "remote"
url = "https://127.0.0.1:$CPPORT"
fingerprint = "$FP"
api_key = "file:$CPSTATE/api-key"
EOF
fi
write_config 24h 10m 0 2

step "first remote build (environment image, machine provisioning, FUSE, outputs)"
out="$(run cargo build 2>&1)"; code=$?
check "cargo build exits 0" eq "$code" 0
check "compiled at the local path" contains "$out" "Compiling hello v0.1.0 ($PROJ)"
check "binary transferred (eager output)" test -x "$PROJ/target/debug/hello"
check "binary runs locally" contains "$("$PROJ/target/debug/hello")" "greeting=hello ran=local"
check "Cargo.lock written back" test -f "$PROJ/Cargo.lock"
MACHINE="$(docker_ ps -q --filter "label=cloudsourced.owner=$OWNER" --filter label=cloudsourced.kind=machine | head -1)"
check "machine provisioning ran" eq "$(docker_ exec "$MACHINE" cat /cache/fleet-provision)" "provisioned-by-fleet"
check "apt packages in environment" contains "$(run sh -c 'jq --version')" "jq-"
check "setup step ran with its inputs" eq "$(run sh -c 'cat /opt/setup-tools.txt')" "jq"
check "environment variables from config" eq "$(run sh -c 'echo $E2E_IMAGE_ENV')" "from-image"
check "toolchain from rust-toolchain.toml" contains "$(run sh -c 'rustc --version')" "$RUSTV"
check "secrets resolved locally and injected" eq "$(run sh -c 'echo "$CMD_SECRET $FILE_SECRET"')" "from-cmd file-secret"
check "secrets never reach the machine's disk" test -z "$(docker_ exec "$MACHINE" grep -rl file-secret /cache /deps 2>/dev/null)"
http_service() { run sh -c 'for i in $(seq 50); do bash -c "exec 3<>/dev/tcp/127.0.0.1/18080 && printf \"GET /hostname HTTP/1.0\r\n\r\n\" >&3 && cat <&3" 2>/dev/null && exit 0; sleep 0.2; done; exit 1'; }
check "service container reachable on localhost" contains "$(http_service)" "200 OK"
check "service runs as its own container next to the machine" eq "$(docker_ ps -q --filter "label=cloudsourced.owner=$OWNER" --filter label=cloudsourced.kind=service | wc -l)" 1

step "incremental build and cargo run"
sed -i 's/greeting=hello/greeting=changed/' "$PROJ/src/main.rs"
out="$(run cargo run 2>&1)"
check "edit picked up immediately" contains "$out" "greeting=changed ran=remote"
check "new binary transferred" contains "$("$PROJ/target/debug/hello")" "greeting=changed"
out="$(run cargo build 2>&1)"
check "no-op build is fresh (warm build cache)" eq "$(echo "$out" | grep -c Compiling)" 0

step "exit codes, streams, stdin, environment, signals"
printf 'pub fn f() { let _x: u32 = "a"; }\n' > "$PROJ/src/broken.rs"
cp "$PROJ/src/main.rs" "$WORK/main.rs.bak"; echo 'mod broken;' >> "$PROJ/src/main.rs"
out="$(run cargo build 2>&1)"; code=$?
check "failing build returns cargo's exit code" eq "$code" 101
check "compiler errors are streamed" contains "$out" "expected \`u32\`, found \`&str\`"
cp "$WORK/main.rs.bak" "$PROJ/src/main.rs"; rm "$PROJ/src/broken.rs"
out="$(run sh -c 'echo to-stdout; echo to-stderr >&2; exit 7' 2>"$WORK/err")"; code=$?
check "exit code forwarded" eq "$code" 7
check "stdout separated" eq "$out" "to-stdout"
check "stderr separated" contains "$(cat "$WORK/err")" "to-stderr"
check "stdin forwarded" eq "$(printf 'a\nb\n' | run sh -c 'wc -l')" "2"
out="$(E2E_VISIBLE=yes HIDDEN_SECRET=no run sh -c 'echo "v=$E2E_VISIBLE s=$HIDDEN_SECRET t=$(env | grep -c CLOUDSOURCED_TOKEN)"')"
check "environment allowlist" eq "$out" "v=yes s= t=0"
(cd "$PROJ" && exec "$BIN" run sh -c 'trap "echo trapped; exit 42" INT; echo ready; sleep 30' > "$WORK/sig" 2>&1) & pid=$!
for _ in $(seq 50); do grep -q ready "$WORK/sig" 2>/dev/null && break; sleep 0.2; done
kill -INT $pid; wait $pid; code=$?
check "SIGINT forwarded to remote process" eq "$code" 42
check "remote trap ran" contains "$(cat "$WORK/sig")" "trapped"

step "write back of remote changes to sources"
mkdir -p "$PROJ/wb"; echo old > "$PROJ/wb/a"; echo keep > "$PROJ/wb/b"; echo gone > "$PROJ/wb/c"
head -c 3000000 /dev/urandom > "$PROJ/wb/big"
bigsum="$(sha256sum < "$PROJ/wb/big")"
out="$(run sh -c 'cd wb && echo new >> a && rm c && mkdir -p d/e && echo deep > d/e/f && ln -s a link && mv b b2 && chmod +x b2 && printf x > t && mv t t2 && sha256sum < big')"
check "large file readable remotely" eq "$out" "$bigsum"
check "append written back" eq "$(cat "$PROJ/wb/a")" "$(printf 'old\nnew')"
check "delete written back" test ! -e "$PROJ/wb/c"
check "nested mkdir written back" eq "$(cat "$PROJ/wb/d/e/f")" "deep"
check "symlink written back" eq "$(readlink "$PROJ/wb/link")" "a"
check "rename written back" test -f "$PROJ/wb/b2" -a ! -e "$PROJ/wb/b"
check "chmod written back" test -x "$PROJ/wb/b2"
check "temp file + rename written back" eq "$(cat "$PROJ/wb/t2")" "x"
check "remote sees its own changes next run" eq "$(run sh -c 'cat wb/d/e/f; test -e wb/c || echo c-gone')" "$(printf 'deep\nc-gone')"

step "projection consistency under rapid local changes"
mkdir -p "$PROJ/cons"
consistent=1
for i in $(seq 1 15); do
  for j in $(seq 1 $((RANDOM % 8 + 1))); do
    f="$PROJ/cons/f$((RANDOM % 12))"
    case $((RANDOM % 4)) in
      0) rm -f "$f" ;;
      1) d="$PROJ/cons/d$((RANDOM % 3))"; mkdir -p "$d"; echo "$RANDOM" > "$d/x" ;;
      *) echo "$i-$j-$RANDOM" > "$f" ;;
    esac
  done
  want="$(cd "$PROJ" && find cons -type f | sort | xargs -r sha256sum)"
  got="$(run sh -c 'find cons -type f | sort | xargs -r sha256sum')"
  if [ "$want" != "$got" ]; then consistent=0; echo "     iteration $i differs"; diff <(echo "$want") <(echo "$got") | head -10; break; fi
done
check "remote view equals local tree immediately after each change" eq "$consistent" 1
check "worker-local build directory is mounted remotely" eq "$(run sh -c 'ls target | grep -c debug')" "1"
echo secret > "$PROJ/ignored.log"; echo '*.log' >> "$PROJ/.gitignore"
check "gitignore rules hide files" eq "$(run sh -c 'test -e ignored.log && echo visible || echo hidden')" "hidden"

step "shims (transparent proxy through PATH)"
cs shim install cargo sh >/dev/null
out="$(cd "$PROJ" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" cargo run -q 2>&1)"
check "cargo via shim runs remotely" contains "$out" "ran=remote"
out="$(cd "$PROJ" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" CLOUDSOURCED_DISABLE=1 sh -c 'echo ${CLOUDSOURCED_SESSION:-local}')"
check "CLOUDSOURCED_DISABLE runs locally" eq "$out" "local"
out="$(cd "$WORK" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" sh -c 'echo ${CLOUDSOURCED_SESSION:-local}')"
check "outside a configured repo the real tool runs" eq "$out" "local"

step "developer experience: port forwarding, cloudsourced why, cost in status"
# curl_local PORT PATH: poll a forwarded local port for up to ~10s.
curl_local() { for _ in $(seq 50); do curl -sf --max-time 2 "http://127.0.0.1:$1$2" && return 0; sleep 0.2; done; return 1; }
(cd "$PROJ" && CLOUDSOURCED_PORTS=18091:18081 exec "$BIN" run sh -c 'busybox httpd -f -p 18081 -h /etc & sleep 8; echo dev-server-done' > "$WORK/fwd" 2>&1) & fpid=$!
got="$(curl_local 18091 /hostname)"
check "dev server in the session reachable on the local port (CLOUDSOURCED_PORTS)" contains "$got" "cs-"
wait $fpid; code=$?
check "forwarded session exits normally" eq "$code" 0
check "forwarded session output intact" contains "$(cat "$WORK/fwd")" "dev-server-done"
check "local port closed when the session ends" test -n "$(curl -s --max-time 2 http://127.0.0.1:18091/hostname >/dev/null || echo closed)"
(cd "$PROJ" && exec "$BIN" run bash -c 'sleep 6' > "$WORK/fwd2" 2>&1) & fpid=$!
got="$(curl_local 18092 /hostname)"
check "service reachable through [tool.bash] ports (local:remote)" contains "$got" "cs-"
wait $fpid
python3 -c 'import socket,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1",18093)); s.listen(); time.sleep(30)' & busypid=$!
sleep 0.5
out="$(cd "$PROJ" && CLOUDSOURCED_PORTS=18093 "$BIN" run sh -c 'echo still-runs' 2>&1)"; code=$?
kill $busypid 2>/dev/null; wait $busypid 2>/dev/null
check "busy local port: clear error" contains "$out" "local port 18093 is already in use"
check "busy local port: run continues" eq "$code" 0
check "busy local port: command output" contains "$out" "still-runs"
run sh -c 'exit 3' >/dev/null 2>&1
why_out="$(cs why)"
check "why shows the last remote run" contains "$why_out" "ran        remote"
check "why shows the command" contains "$(echo "$why_out" | head -1)" "sh -c exit 3"
check "why shows the exit code" contains "$why_out" "exit       3"
check "why shows machine, size and price" contains "$why_out" "(local/default, 0.500/h)"
check "why shows the placement reason" contains "$why_out" "placement"
check "why shows timings" contains "$why_out" "queue / machine start"
wj="$(cs why --json)"
check "why --json has exit, timings and usage" eq "$(echo "$wj" | jq -r '[.mode, .exit, (.timings.total_ms > 0), (.usage != null), .provider, .size] | join(" ")')" "remote 3 true true local default"
(cd "$PROJ" && CLOUDSOURCED_DISABLE=1 "$BIN" run sh -c true)
check "why records local decisions" contains "$(cs why last)" "local: CLOUDSOURCED_DISABLE is set"
check "why N addresses older runs" contains "$(cs why 2)" "sh -c exit 3"
check "why list shows recent runs" eq "$(cs why list | sed -n 2p | awk '{print $1, $5}')" "1 local"
st="$(cs status)"
check "status shows price per size and a total" contains "$st" "local/default  0.500"
check "status total line" contains "$st" "  total"
sj="$(cs status --json)"
check "status --json reports cost from price_per_hour" eq "$(echo "$sj" | jq -r '[.cost.sizes[0].price_per_hour, (.cost.total > 0), (.cost.per_hour > 0), (.machines | length > 0)] | join(" ")')" "0.5 true true true"

step "editor integration (rust-analyzer flycheck through the proxy)"
out="$(cd "$PROJ" && "$BIN" run cargo check --workspace --message-format=json 2>/dev/null)"
check "JSON diagnostics on stdout with local paths" contains "$(echo "$out" | head -1)" "\"reason\":\"compiler-artifact\""
check "check reports the local manifest path" contains "$out" "\"manifest_path\":\"$PROJ/Cargo.toml\""

step "lazy outputs"
rm -f "$PROJ/target/debug/hello.d"
out="$(cd "$PROJ" && cs fetch 'target/debug/*.d' 2>&1)"
check "fetch copies outputs from the latest build" test -f "$PROJ/target/debug/hello.d"

step "dependencies (shared registry cache)"
cat >> "$PROJ/Cargo.toml" <<'EOF'
itoa = "1"
EOF
sed -i 's/fn main() {/fn main() {\n    let mut b = itoa::Buffer::new(); println!("itoa={}", b.format(42));/' "$PROJ/src/main.rs"
out="$(run cargo run -q 2>&1)"
check "crate downloaded and used remotely" contains "$out" "itoa=42"
check "Cargo.lock updated by remote cargo" contains "$(cat "$PROJ/Cargo.lock")" 'name = "itoa"'
check "registry cache stored on the deps volume" contains "$(docker_ exec "$MACHINE" sh -c 'ls /deps/usr__local__cargo__registry/cache/*/')" "itoa-"

step "compiler cache (sccache) and linker (mold)"
check "sessions use sccache as rustc wrapper" eq "$(run sh -c 'echo $RUSTC_WRAPPER')" "sccache"
check "binary linked with mold" contains "$(run sh -c 'readelf -p .comment target/debug/hello')" "mold"
run cargo clean -q -p itoa >/dev/null 2>&1
out="$(CLOUDSOURCED_VERBOSE=1 run cargo build 2>&1)"
check "cleaned dependency recompiled" contains "$out" "Compiling itoa"
check "recompiled dependency served from the compiler cache" test "$(sccache_hits "$out")" -ge 1
check "compiler cache entries on the shared dependency volume" test "$(docker_ exec "$MACHINE" sh -c 'find /deps/sccache -type f | wc -l')" -ge 1

step "parallel sessions: one machine runs several, scale out beyond capacity"
pids=""
for i in 1 2 3; do (run sh -c "sleep 6; hostname" > "$WORK/par$i" 2>&1) & pids="$pids $!"; done
sleep 3
status="$(cs status)"
wait $pids
machines="$(cat "$WORK"/par* | grep '^cs-' | sort -u | wc -l)"
sessions_max="$(echo "$status" | awk '/^[0-9a-f]+ /{print $(NF-2)}' | sort -n | tail -1)"
check "two 2G sessions share one 4G machine" eq "$sessions_max" 2
check "third session scaled out to a second machine" eq "$machines" 2
check "parallel sessions succeed" eq "$(cat "$WORK"/par* | grep -c '^cs-')" 3

step "lifecycle: idle stop, warm restart, lifetime expiry, cleanup"
cs down >/dev/null
write_config 75s 10s 0 2; restart_daemon
run cargo build -q >/dev/null 2>&1
M="$(docker_ ps -q --filter "label=cloudsourced.owner=$OWNER" --filter label=cloudsourced.kind=machine | head -1)"
NAME="$(docker_ inspect -f '{{.Name}}' "$M" | tr -d /)"
for _ in $(seq 60); do [ "$(docker_ inspect -f '{{.State.Status}}' "$NAME")" = exited ] && break; sleep 1; done
check "idle machine stopped after idle_stop" eq "$(docker_ inspect -f '{{.State.Status}}' "$NAME")" exited
check "stopped machine keeps its cache volume" test -n "$(docker_ volume ls -q --filter "name=cs-cache-${NAME#cs-}")"
out="$(CLOUDSOURCED_VERBOSE=1 run cargo build 2>&1)"
check "stopped machine restarted on demand" contains "$out" "starting machine ${NAME#cs-}"
check "restarted machine still has a warm build cache" eq "$(echo "$out" | grep -c Compiling)" 0
check "services restart with their machine" contains "$(http_service)" "200 OK"
for _ in $(seq 120); do [ -z "$(docker_ ps -aq --filter "name=^$NAME\$")" ] && break; sleep 1; done
check "machine destroyed after max_lifetime" test -z "$(docker_ ps -aq --filter "name=^$NAME\$")"
for _ in $(seq 30); do [ -z "$(docker_ volume ls -q --filter "name=cs-cache-${NAME#cs-}")" ] && break; sleep 1; done
check "its build cache volume is gone" test -z "$(docker_ volume ls -q --filter "name=cs-cache-${NAME#cs-}")"
out="$(CLOUDSOURCED_VERBOSE=1 run cargo build 2>&1)"
check "fresh machine builds with an empty build cache" contains "$out" "Compiling itoa"
check "fresh machine gets compiler cache hits for dependencies" test "$(sccache_hits "$out")" -ge 1

step "fail open when no machine can be provisioned"
write_config 24h 10m 0 2
sed -i 's/^command = .*/command = ["false"]/' "$FLEETCFG"; restart_daemon
out="$(cd "$PROJ" && CLOUDSOURCED_REQUIRE_REMOTE= "$BIN" run sh -c 'echo ${CLOUDSOURCED_SESSION:-local}' 2>&1)"
check "falls back to local execution" contains "$out" "local"
(cd "$PROJ" && "$BIN" run sh -c true >/dev/null 2>&1); code=$?
check "CLOUDSOURCED_REQUIRE_REMOTE turns fallback into an error" eq "$code" 125
write_config 24h 10m 0 2; restart_daemon
