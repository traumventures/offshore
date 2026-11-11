#!/usr/bin/env bash
# Smoke test of the control plane container with the local-docker provider:
# start the image like docs/control-plane.md describes, point a client at it
# (remote mode, pinned fingerprint) and run "cargo build" of the e2e fixture
# on a machine the containerised control plane created. Cleans up after
# itself (containers, volumes, state); the image is kept.
#
#   make build image && DOCKER="sudo -n docker" deploy/docker/smoke-test.sh
#
# The state directory is bind mounted at a different path than on the host
# on purpose: the control plane must translate the path of the server binary
# it exports for local-docker machines.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
BIN="${SMOKE_BIN:-$REPO/bin/offshore}"
DOCKER="${DOCKER:-docker}"
IMAGE="${IMAGE:-offshore-controlplane:latest}"
PORT="${SMOKE_PORT:-17443}"
WORK="$(mktemp -d /tmp/cs-cp-smoke.XXXXXX)"
NAME="cs-cp-smoke-$(basename "$WORK" | tr -dc 'a-z0-9' | tail -c 6)"
OWNER="smoke-${NAME##*-}"
HOSTSTATE="$WORK/cp-state"
d() { $DOCKER "$@"; }
PASS=0 FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $*"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $*"; }

cleanup() {
  echo "== cleanup"
  (cd "$WORK/hello" 2>/dev/null && OFFSHORE_CONFIG="$WORK/client.toml" OFFSHORE_STATE_DIR="$WORK/client" "$BIN" down >/dev/null 2>&1)
  OFFSHORE_CONFIG="$WORK/client.toml" OFFSHORE_STATE_DIR="$WORK/client" "$BIN" daemon stop >/dev/null 2>&1
  d logs "$NAME" > "$WORK/cp.log" 2>&1
  d stop -t 30 "$NAME" >/dev/null 2>&1; d rm -f "$NAME" >/dev/null 2>&1
  for c in $(d ps -aq --filter "label=offshore.pool=$OWNER"); do d rm -f -v "$c" >/dev/null; done
  for v in $(d volume ls -q --filter "label=offshore.pool=$OWNER"); do d volume rm -f "$v" >/dev/null; done
  left=$(( $(d ps -aq --filter "label=offshore.pool=$OWNER" | wc -l) + $(d volume ls -q --filter "label=offshore.pool=$OWNER" | wc -l) ))
  [ "$left" = 0 ] && ok "no machines or volumes left" || bad "$left resources left"
  # The state belongs to uid 10001 (the container user).
  d run --rm -v "$WORK:/w" busybox:1.37 rm -rf /w/cp-state >/dev/null 2>&1
  [ -n "${SMOKE_KEEP:-}" ] || python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1], ignore_errors=True)' "$WORK"
  echo "$PASS passed, $FAIL failed"
  [ "$FAIL" = 0 ]
}
trap 'cleanup; exit $?' EXIT

cp -r "$REPO/test/e2e/fixture/hello" "$WORK/hello"
echo file-secret > "$WORK/hello/.secret"
RUSTV="$(d run --rm rust:1-slim-bookworm rustc --version | awk '{print $2}')"
printf '[toolchain]\nchannel = "%s"\nprofile = "minimal"\n' "$RUSTV" > "$WORK/hello/rust-toolchain.toml"

echo "== start $IMAGE as $NAME"
mkdir -p "$HOSTSTATE"
cat > "$HOSTSTATE/config.toml" <<CFG
[pool]
id = "$OWNER"
max_machines = 1

[providers.local]
type = "local-docker"
command = ["docker"]

[scheduler]
estimator = "static"

[[providers.local.size]]
name = "small"
cpu = 4
memory = "4G"
CFG
d run --rm -v "$HOSTSTATE:/s" busybox:1.37 chown -R 10001:10001 /s
SOCK_GID="$(d run --rm -v /var/run/docker.sock:/var/run/docker.sock busybox:1.37 stat -c %g /var/run/docker.sock)"
d run -d --name "$NAME" --network host --group-add "$SOCK_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$HOSTSTATE:/var/lib/offshore" \
  "$IMAGE" controlplane --listen "127.0.0.1:$PORT" >/dev/null || exit 1
for _ in $(seq 100); do curl -skf "https://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 0.2; done
curl -skf "https://127.0.0.1:$PORT/healthz" >/dev/null && ok "healthz" || { bad "healthz"; d logs "$NAME"; exit 1; }
d exec "$NAME" offshore controlplane health >/dev/null && ok "controlplane health in the container" || bad "controlplane health"
logs="$(d logs "$NAME" 2>&1)"
case "$logs" in *"machine bin: $HOSTSTATE/bin/offshore"*) ok "machine binary mapped to the host path";; *) bad "machine binary not mapped: $logs";; esac
case "$logs" in *cs_*) bad "API key in the logs";; *) ok "API key not logged";; esac
info="$(d exec "$NAME" offshore controlplane info)"
case "$info" in *cs_*) bad "info prints the key without --show-key";; *) ok "info hides the key";; esac
KEY="$(d exec "$NAME" offshore controlplane info --show-key | sed -n 's/^api key value: //p')"
[ -n "$KEY" ] && ok "info --show-key" || bad "info --show-key"
d exec "$NAME" offshore controlplane client-config --url "https://127.0.0.1:$PORT" > "$WORK/client.toml"
grep -q '^fingerprint = "[0-9a-f]\{64\}"' "$WORK/client.toml" && ok "client-config pins the fingerprint" || bad "client-config: $(cat "$WORK/client.toml")"
# The same block, fetched from the outside (trust on first use).
remote="$("$BIN" controlplane client-config --state-dir "$WORK/none" --url "https://127.0.0.1:$PORT")"
[ "$(grep fingerprint <<<"$remote")" = "$(grep fingerprint "$WORK/client.toml")" ] && ok "fetched fingerprint matches" || bad "fetched fingerprint differs"

echo "== remote build"
export OFFSHORE_CONFIG="$WORK/client.toml" OFFSHORE_STATE_DIR="$WORK/client" OFFSHORE_API_KEY="$KEY"
export OFFSHORE_REQUIRE_REMOTE=1 OFFSHORE_SHIM_DIR="$WORK/shims"
unset OFFSHORE_DISABLE OFFSHORE_ACTIVE
out="$(cd "$WORK/hello" && "$BIN" run cargo build 2>&1)"; code=$?
[ "$code" = 0 ] && ok "cargo build exits 0" || bad "cargo build exit $code: $(tail -20 <<<"$out")"
case "$out" in *"Compiling hello v0.1.0 ($WORK/hello)"*) ok "compiled remotely at the local path";; *) bad "no compile line";; esac
[ -x "$WORK/hello/target/debug/hello" ] && ok "binary transferred" || bad "binary missing"
"$WORK/hello/target/debug/hello" | grep -q 'ran=local' && ok "binary runs locally" || bad "binary output"
m="$(d ps -q --filter "label=offshore.pool=$OWNER" --filter label=offshore.kind=machine | head -1)"
[ -n "$m" ] && ok "machine created by the containerised control plane" || bad "no machine"
src="$(d inspect -f '{{range .Mounts}}{{if eq .Destination "/opt/offshore/bin/offshore"}}{{.Source}}{{end}}{{end}}' "$m" 2>/dev/null)"
[ "$src" = "$HOSTSTATE/bin/offshore" ] && ok "machine mounts the exported binary" || bad "machine binary source: $src"
