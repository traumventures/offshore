#!/usr/bin/env bash
# End-to-end test of cloudsourced with the local-docker backend.
#
# Everything is real: environment image build, machine provisioning as
# Docker containers, FUSE projection, sessions, write back, outputs,
# scheduling, scale out, idle stop, lifetime expiry and cleanup. Every part
# (parts/*.sh) uses its own owner, state directory, daemon, test clock and
# local ports, so it does not touch an existing setup or another part.
#
# Requirements: docker (set E2E_DOCKER="sudo -n docker" if your user is not
# in the docker group), /dev/fuse, a static build at bin/cloudsourced.
#
#   E2E_REMOTE=1    run against a shared control plane ("cloudsourced
#                   controlplane", self-signed TLS, API key) instead of the
#                   orchestrator embedded in the daemon. Only the core part
#                   runs (it covers what differs: control plane API,
#                   leases, metrics, request ids) unless E2E_FULL=1.
#   E2E_FULL=1      every part in remote mode too
#   E2E_PARTS="core rust lifecycle"   the parts to run (default: all;
#                   remote: core)
#   E2E_PARALLEL=1  run the parts concurrently; the first one builds the
#                   environment image, the others start once it exists
#   E2E_KEEP=1      keep the work directories (state, logs)
#   E2E_CLEAN_IMAGES=1  remove environment images the run built (they are
#                   reused across runs by default)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DOCKER="${E2E_DOCKER:-docker}"
ALL="core rust lifecycle"
if [ -n "${E2E_PARTS:-}" ]; then
  PARTS="$E2E_PARTS"
elif [ -n "${E2E_REMOTE:-}" ] && [ -z "${E2E_FULL:-}" ]; then
  PARTS="core"
else
  PARTS="$ALL"
fi
for p in $PARTS; do [ -f "$HERE/parts/$p.sh" ] || { echo "unknown part $p (have: $ALL)"; exit 2; }; done

images() { $DOCKER images --format '{{.Repository}}:{{.Tag}}' 'cloudsourced-env' 2>/dev/null | sort; }
before="$(images)"
# Resolved once for all parts (a container start each otherwise).
E2E_RUSTV="$($DOCKER run --rm rust:1-slim-bookworm rustc --version | awk '{print $2}')" || exit 2
export E2E_RUSTV

RES="$(mktemp -d /tmp/cloudsourced-e2e-run.XXXXXX)"
start=$SECONDS
mode=embedded; [ -n "${E2E_REMOTE:-}" ] && mode=remote
echo "e2e ($mode): parts: $PARTS$([ -n "${E2E_PARALLEL:-}" ] && echo ' (parallel)')"

runpart() { # part: runs it, output to $RES/part.log (parallel) or the terminal
  local p="$1"
  if [ -n "${E2E_PARALLEL:-}" ]; then
    E2E_RESULT="$RES/$p.result" E2E_READY="$RES/ready" bash "$HERE/parts/$p.sh" > "$RES/$p.log" 2>&1
  else
    E2E_RESULT="$RES/$p.result" bash "$HERE/parts/$p.sh"
  fi
}

if [ -n "${E2E_PARALLEL:-}" ]; then
  set -- $PARTS
  runpart "$1" & pids="$!"; first=$!
  # Further parts wait for the environment image (or the first part's end).
  while [ ! -e "$RES/ready" ] && kill -0 "$first" 2>/dev/null; do sleep 1; done
  shift
  for p in "$@"; do runpart "$p" & pids="$pids $!"; done
  wait $pids
  for p in $PARTS; do printf '\n######## %s\n' "$p"; cat "$RES/$p.log"; done
else
  for p in $PARTS; do runpart "$p"; done
fi

pass=0; fail=0
echo
for p in $PARTS; do
  if [ -f "$RES/$p.result" ]; then
    read -r a b t < "$RES/$p.result"
  else
    a=0; b=1; t="?"; echo "  $p: no result (aborted)"
  fi
  printf '  %-10s %3d passed, %d failed (%ss)\n' "$p" "$a" "$b" "$t"
  pass=$((pass + a)); fail=$((fail + b))
done
if [ -n "${E2E_CLEAN_IMAGES:-}" ]; then
  for img in $(comm -13 <(echo "$before") <(images)); do $DOCKER rmi "$img" >/dev/null && echo "removed $img"; done
fi
python3 -c 'import shutil, sys; shutil.rmtree(sys.argv[1], True)' "$RES"
printf '\n%d passed, %d failed (%s, %ds)\n' "$pass" "$fail" "$mode" $((SECONDS - start))
[ "$fail" = 0 ]
