#!/usr/bin/env bash
# Host wrapper for the storage-tier FUSE benchmark (measurement spike, not CI).
# Builds tierfs, checks out cargo-nextest, starts a privileged rust container
# and runs the layouts sequentially (nice -n 10). Results: $WORK/results.jsonl.
#
#   test/bench/tiers/run.sh                 # all layouts
#   test/bench/tiers/run.sh layout_A        # one layout (see bench.sh)
#   test/bench/tiers/run.sh alt 5 A E       # A and E alternating in one window
#   KEEP=1 test/bench/tiers/run.sh ...      # keep container + work dir
#
# Needs: sudo -n docker, /dev/fuse, ~12 GB free disk (target ~3.2 GB, lower
# store is hardlinked into A's target, uppers/caches up to ~2 GB each).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
WORK=${WORK:-$HOME/tierbench}
IMAGE=${IMAGE:-rust:1-slim-bookworm}
NEXTEST_REV=${NEXTEST_REV:-2cf55ec}
D="sudo -n docker"
mkdir -p "$WORK"
(cd "$repo" && CGO_ENABLED=0 go build -o "$WORK/tierfs" ./test/bench/tiers/tierfs && CGO_ENABLED=0 go build -o "$WORK/anyfsbench" ./test/bench/tiers/anyfsbench)
cp "$here/bench.sh" "$here/check.sh" "$WORK/"
if [ ! -d "$WORK/src" ]; then
  git clone -q https://github.com/nextest-rs/nextest "$WORK/src"
  git -C "$WORK/src" checkout -q "$NEXTEST_REV"
fi
$D rm -f tierbench >/dev/null 2>&1 || true
$D run -d --name tierbench --privileged --device /dev/fuse -v "$WORK:/work" \
  -e CARGO_HOME=/work/cargo-home -w /work/src "$IMAGE" sleep infinity >/dev/null
$D exec tierbench bash -c 'command -v python3 >/dev/null || (apt-get -qq update && apt-get -qq install -y python3 >/dev/null)'
$D exec tierbench cargo fetch --locked
ovl=",redirect_dir=on,metacopy=on"
run() { $D exec ${OVL:+-e OVL_OPTS=$OVL} tierbench bash /work/bench.sh "$@"; }
out="$here/results-$(date +%F).jsonl"
if [ "${1:-}" = alt ]; then  # alt ROUNDS TAG...: alternating A/E/... in one window (bench.sh layout_alt)
  shift; out="$here/results-alt-$(date +%F-%H%M).jsonl"
  [ -f "$WORK/s1/leaf" ] || run layout_A
  run layout_alt "$@"
elif [ $# -gt 0 ]; then run "$@"; else
  run layout_A                       # baseline; its final target becomes the lower
  run layout_C Cwb -writeback        # rw FUSE, writeback cache (passthrough refused)
  run layout_C Cpt                   # rw FUSE, passthrough
  run layout_C Cptx -noflush -noxattr
  run layout_Bclean                  # overlay over empty FUSE lower, clean build
  run layout_B B                     # overlay defaults (full copy-up)
  OVL=,volatile run layout_B Bvol
  OVL=$ovl run layout_B Bmeta        # metacopy: copy-up is metadata only
  OVL=$ovl run layout_B Bnopt -passthrough=false
  OVL=$ovl run layout_D D            # lazy fetch, 5 ms/object, 200 MB/s
  OVL=$ovl run layout_D Dpf -prefetch-max-size 65536
  run layout_E                       # anyfs (real implementation), warm slot
  run layout_F                       # anyfs, cold lazy restore from a slow store
  $D exec tierbench bash /work/check.sh
  run layout_freshclone              # last: touches every source file
fi
cp "$WORK/results.jsonl" "$out" 2>/dev/null || true
if [ -z "${KEEP:-}" ]; then
  $D exec tierbench python3 -c "import shutil; shutil.rmtree('/work', ignore_errors=True)" || true
  $D rm -f tierbench >/dev/null
fi
