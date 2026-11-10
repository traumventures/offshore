#!/usr/bin/env bash
# core: sessions without compiling: configuration, environment, exec,
# streams, signals, write back, projection, shims, ports, why, status,
# metrics, machine authentication, parallel sessions and scale out.
PART=core
. "$(dirname "$0")/../lib.sh"

step "configuration roles"
out="$(cd "$WORK" && cs config check 2>&1)"; code=$?
check "config check exits 0" eq "$code" 0
check "client configuration has the client role" contains "$out" "client configuration ok: $CLOUDSOURCED_CONFIG"
if [ -n "$REMOTE" ]; then
  check "remote client does not use a fleet configuration" contains "$out" "fleet configuration: not used by this client"
  out="$(cs config check --fleet "$FLEETCFG" 2>&1)"
  check "control plane fleet file validates" contains "$out" "role: control plane"
  check "control plane fleet file has no deprecated keys" eq "$(echo "$out" | grep -c deprecated)" 0
else
  check "embedded orchestrator reads the same file" contains "$out" "fleet configuration ok: $FLEETCFG"
  check "legacy keys are reported as deprecated" contains "$out" "fleet.owner is deprecated"
fi
check "config show prints the effective configuration" contains "$(cd "$WORK" && cs config show 2>&1)" "[orchestrator]"

step "first remote session (environment image, machine provisioning, services)"
out="$(run sh -c 'echo first-session' 2>&1)"; code=$?
check "first session exits 0" eq "$code" 0
check "first session output" contains "$out" "first-session"
ready
MACHINE="$(machines | head -1)"
check "machine provisioning ran" eq "$(docker_ exec "$MACHINE" cat /cache/fleet-provision)" "provisioned-by-fleet"
check "apt packages in environment" contains "$(run sh -c 'jq --version')" "jq-"
check "setup step ran with its inputs" eq "$(run sh -c 'cat /opt/setup-tools.txt')" "jq"
check "environment variables from config" eq "$(run sh -c 'echo $E2E_IMAGE_ENV')" "from-image"
check "toolchain from rust-toolchain.toml" contains "$(run sh -c 'rustc --version')" "$RUSTV"
check "secrets resolved locally and injected" eq "$(run sh -c 'echo "$CMD_SECRET $FILE_SECRET"')" "from-cmd file-secret"
check "secrets never reach the machine's disk" test -z "$(docker_ exec "$MACHINE" grep -rl file-secret /cache /deps 2>/dev/null)"
check "service container reachable on localhost" contains "$(http_service)" "200 OK"
check "service runs as its own container next to the machine" eq "$(docker_ ps -q --filter "label=cloudsourced.owner=$OWNER" --filter label=cloudsourced.kind=service | wc -l)" 1

step "exit codes, streams, stdin, environment, signals"
out="$(run sh -c 'echo to-stdout; echo to-stderr >&2; exit 7' 2>"$WORK/err")"; code=$?
check "exit code forwarded" eq "$code" 7
check "stdout separated" eq "$out" "to-stdout"
check "stderr separated" contains "$(cat "$WORK/err")" "to-stderr"
check "stdin forwarded" eq "$(printf 'a\nb\n' | run sh -c 'wc -l')" "2"
out="$(E2E_VISIBLE=yes HIDDEN_SECRET=no run sh -c 'echo "v=$E2E_VISIBLE s=$HIDDEN_SECRET t=$(env | grep -c -e CLOUDSOURCED_TLS_SEED -e CLOUDSOURCED_SIGNER_PUB)"')"
check "environment allowlist" eq "$out" "v=yes s= t=0"
(cd "$PROJ" && exec "$BIN" run sh -c 'trap "echo trapped; exit 42" INT; echo ready; sleep 60' > "$WORK/sig" 2>&1) & pid=$!
poll 60 grep -q ready "$WORK/sig" >/dev/null
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
echo secret > "$PROJ/ignored.log"; echo '*.log' >> "$PROJ/.gitignore"
check "gitignore rules hide files" eq "$(run sh -c 'test -e ignored.log && echo visible || echo hidden')" "hidden"

step "shims (transparent proxy through PATH)"
cs shim install sh >/dev/null
out="$(cd "$PROJ" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" sh -c 'echo ${CLOUDSOURCED_SESSION:+ran=remote}')"
check "sh via shim runs remotely" eq "$out" "ran=remote"
out="$(cd "$PROJ" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" CLOUDSOURCED_DISABLE=1 sh -c 'echo ${CLOUDSOURCED_SESSION:-local}')"
check "CLOUDSOURCED_DISABLE runs locally" eq "$out" "local"
out="$(cd "$WORK" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" sh -c 'echo ${CLOUDSOURCED_SESSION:-local}')"
check "outside a configured repo the real tool runs" eq "$out" "local"

step "developer experience: port forwarding, cloudsourced why, cost in status"
FWD_ENV="$(freeport)"; FWD_BUSY="$(freeport)"
# curl_local PORT PATH: poll a forwarded local port for up to 60s (session
# start on a loaded host).
curl_local() { for _ in $(seq 300); do curl -sf --max-time 2 "http://127.0.0.1:$1$2" && return 0; sleep 0.2; done; return 1; }
# The sessions last until the test releases them (see wait_release).
(cd "$PROJ" && CLOUDSOURCED_PORTS=$FWD_ENV:18081 exec "$BIN" run sh -c 'busybox httpd -f -p 18081 -h /etc & bash -c "$1"; echo dev-server-done' _ "$(wait_release fwd-done)" > "$WORK/fwd" 2>&1) & fpid=$!
got="$(curl_local $FWD_ENV /hostname)"
check "dev server in the session reachable on the local port (CLOUDSOURCED_PORTS)" contains "$got" "cs-"
release fwd-done
wait $fpid; code=$?
check "forwarded session exits normally" eq "$code" 0
check "forwarded session output intact" contains "$(cat "$WORK/fwd")" "dev-server-done"
check "local port closed when the session ends" test -n "$(curl -s --max-time 2 http://127.0.0.1:$FWD_ENV/hostname >/dev/null || echo closed)"
(cd "$PROJ" && exec "$BIN" run bash -c "$(wait_release svc-done)" > "$WORK/fwd2" 2>&1) & fpid=$!
got="$(curl_local $FWD_SVC /hostname)"
check "service reachable through [tool.bash] ports (local:remote)" contains "$got" "cs-"
release svc-done; wait $fpid
python3 -c 'import socket,sys,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1",int(sys.argv[1]))); s.listen(); time.sleep(300)' "$FWD_BUSY" & busypid=$!
poll 10 sh -c "ss -ltn | grep -q '127.0.0.1:$FWD_BUSY '"
out="$(cd "$PROJ" && CLOUDSOURCED_PORTS=$FWD_BUSY "$BIN" run sh -c 'echo still-runs' 2>&1)"; code=$?
kill $busypid 2>/dev/null; wait $busypid 2>/dev/null
check "busy local port: clear error" contains "$out" "local port $FWD_BUSY is already in use"
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

step "operations: metrics and structured logs"
MLABEL="$(docker_ inspect -f '{{index .Config.Labels "cloudsourced.machine"}}' "$MACHINE")"
m="$(cs metrics)"
check "metrics: daemon counts prepares" test "$(metric "$m" cloudsourced_daemon_requests_total 'op="prepare",result="ok"')" -ge 1
check "metrics: fleet machines by provider, size and state" contains "$m" 'cloudsourced_machines{provider="local",size="default",state="running"} 1'
check "metrics: machine session exits with machine label" poll 30 sh -c 'case "$("$1" metrics)" in *"$2"*) ;; *) exit 1;; esac' _ "$BIN" "cloudsourced_server_session_exits_total{machine=\"$MLABEL\",provider=\"local\",size=\"default\",code=\"7\"}"
check "metrics: acquire wait histogram" test "$(metric "$m" cloudsourced_acquire_wait_seconds_count 'result="assigned"')" -ge 1
check "metrics: cost per hour of running machines" contains "$m" 'cloudsourced_cost_per_hour{provider="local"} 0.5'
check "machine server logs JSON (format forwarded by the orchestrator)" contains "$(docker_ logs "$MACHINE" 2>&1 | grep '"msg":"session finished"' | tail -1)" '"level":"INFO"'
if [ -n "$REMOTE" ]; then
  KEY="$(cat "$CPSTATE/api-key")"
  check "control plane /metrics needs the API key" eq "$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:$CPPORT/metrics")" 401
  cm="$(curl -sk -H "Authorization: Bearer $KEY" "https://127.0.0.1:$CPPORT/metrics")"
  check "control plane /metrics has machine and session metrics" contains "$cm" "cloudsourced_server_session_duration_seconds_count{machine=\"$MLABEL\""
  check "control plane /metrics counts API requests" test "$(metric "$cm" cloudsourced_http_requests_total 'path="/v1/acquire",result="200"')" -ge 1
  check "--metrics-listen serves /metrics without authentication" contains "$(curl -s "http://127.0.0.1:$MPORT/metrics")" "cloudsourced_queue_length"
  check "control plane logs requests as JSON with request ids" contains "$(grep '"msg":"lease"' "$WORK/cp.log" | tail -1)" '"request_id":"'
else
  check "daemon logs JSON" contains "$(grep '"msg":"daemon started"' "$CLOUDSOURCED_STATE_DIR/daemon.log" | tail -1)" '"level":"INFO"'
fi

step "machine authentication (derived identity, signed scoped tokens)"
MDIR="$CLOUDSOURCED_STATE_DIR"; [ -n "$REMOTE" ] && MDIR="$CPSTATE"
label() { docker_ inspect -f "{{index .Config.Labels \"$1\"}}" "$MACHINE"; }
MID="$(label cloudsourced.machine)"; MFP="$(label cloudsourced.fingerprint)"
MADDR="$(docker_ port "$MACHINE" 7443/udp | head -1)"
SEED="$(docker_ inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$MACHINE" | sed -n 's/^CLOUDSOURCED_TLS_SEED=//p')"
probe() { cs debug probe "$MADDR" "$MFP" "$1" 2>&1; }
token() { cs debug token --state-dir "$MDIR" --machine "$MID" "$@"; }
check "fleet master key stored with mode 600" eq "$(stat -c %a "$MDIR/master.key")" 600
check "no per-machine secret files" test -z "$(ls "$MDIR/machines" 2>/dev/null)"
check "machine got only a TLS seed as secret" test -n "$SEED"
check "probe token reads machine status" contains "$(probe "$(token --scope probe)")" "\"machine\": \"$MID\""
check "token signed by another key rejected" contains "$(probe "$(CLOUDSOURCED_MASTER_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" cs debug token --machine "$MID" --scope admin)")" "invalid token: bad signature"
check "token for another machine rejected" contains "$(probe "$(cs debug token --state-dir "$MDIR" --machine other --scope probe)")" "another machine"
check "expired token rejected" contains "$(probe "$(token --scope probe --ttl -10m)")" "expired"
check "session token cannot probe" contains "$(probe "$(token --scope session --lease l1)")" "does not allow status"
check "garbage token rejected" contains "$(probe "not-a-token")" "invalid token"
if [ -n "$REMOTE" ]; then
  st="$(curl -sk -X POST -H "Authorization: Bearer $(cat "$CPSTATE/api-key")" "https://127.0.0.1:$CPPORT/v1/status")"
else
  st="$(cs status)"
fi
check "status carries no tokens or seeds" sh -c 'case "$1" in *Token*|*"$2"*) exit 1;; esac' _ "$st" "$SEED"

step "parallel sessions: one machine runs several, scale out beyond capacity"
# The sessions wait until the test releases them, so the three overlap
# however slowly sessions and the second machine start. Released once the
# status shows two sessions on one machine and three in total (or after
# 120s; then the checks below fail).
pids=""
for i in 1 2 3; do (run sh -c 'bash -c "$1"; hostname' _ "$(wait_release par-done)" > "$WORK/par$i" 2>&1) & pids="$pids $!"; done
sessions_max=0
for _ in $(seq 240); do
  read -r mx total <<<"$(cs status 2>/dev/null | awk '/^[0-9a-f]+ / {n=$(NF-2); t+=n; if (n>m) m=n} END {print m+0, t+0}')"
  [ "${mx:-0}" -gt "$sessions_max" ] && sessions_max="$mx"
  [ "$sessions_max" -ge 2 ] && [ "${total:-0}" -ge 3 ] && break
  sleep 0.5
done
release par-done
poll 30 no_sessions >/dev/null || release par-done
wait $pids
machines="$(cat "$WORK"/par* | grep '^cs-' | sort -u | wc -l)"
check "two 2G sessions share one 4G machine" eq "$sessions_max" 2
check "third session scaled out to a second machine" eq "$machines" 2
check "parallel sessions succeed" eq "$(cat "$WORK"/par* | grep -c '^cs-')" 3
