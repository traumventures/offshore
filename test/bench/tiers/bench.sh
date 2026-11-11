#!/usr/bin/env bash
# Runs INSIDE the privileged bench container (see run.sh). Layout functions
# mount /t (the CARGO_TARGET_DIR) in different ways and run build steps;
# every step appends one JSON line to $RESULTS.
set -euo pipefail
W=/work
SRC=$W/src
RUNS=$W/runs
RESULTS=${RESULTS:-$W/results.jsonl}
JOBS=${JOBS:-8}
TIERFS=$W/tierfs
ANYFS=$W/anyfsbench
LEAF=cargo-nextest/src/main.rs
CORE=nextest-metadata/src/lib.rs
export CARGO_TARGET_DIR=/t CARGO_INCREMENTAL=1 CARGO_TERM_COLOR=never
unset RUSTC_WRAPPER SCCACHE_DIR || true
mkdir -p /t "$RUNS"

statsfile=""
fusepid=""
snap() { # write daemon stats to $1 (or "{}" when no FUSE daemon)
  if [ -n "$fusepid" ]; then
    rm -f "$statsfile"; kill -USR1 "$fusepid"
    for _ in $(seq 100); do [ -s "$statsfile" ] && break; sleep 0.05; done
    cp "$statsfile" "$1"
  else echo '{}' > "$1"; fi
}

# pristine copies of the edited files (cp -p keeps mtimes)
[ -f $W/pristine/leaf ] || { mkdir -p $W/pristine; cp -p $SRC/$LEAF $W/pristine/leaf; cp -p $SRC/$CORE $W/pristine/core; }
reset_src() { cp -p $W/pristine/leaf $SRC/$LEAF; cp -p $W/pristine/core $SRC/$CORE; }
n=0
edit() { n=$((n+1)); echo "// bench edit $n $(date +%s%N)" >> "$SRC/$1"; }

step() { # step LAYOUT NAME
  local layout=$1 name=$2 log=$W/logs/$1-$2.log
  mkdir -p $W/logs
  snap /tmp/before.json
  local t0 t1
  t0=$(date +%s.%N)
  local TIMEFORMAT='%R %U %S'
  { time (cd $SRC && nice -n 10 cargo build -p cargo-nextest --locked --offline -j "$JOBS" > "$log" 2>&1); } 2> /tmp/time.txt || { echo "BUILD FAILED $layout $name"; tail -30 "$log"; exit 1; }
  snap /tmp/after.json
  local compiled; compiled=$(grep -c '^ *Compiling ' "$log" || true)
  local upper=0; [ -d "$RUNS/$layout/upper" ] && upper=$(du -s --block-size=1 "$RUNS/$layout/upper" | cut -f1)
  python3 - "$layout" "$name" "$compiled" "$upper" <<'PY' >> "$RESULTS"
import json,sys
real,user,sys_=open('/tmp/time.txt').read().split()
b=json.load(open('/tmp/before.json')); a=json.load(open('/tmp/after.json'))
d={}
for k,v in a.items():
    if isinstance(v,dict):  # per-op histograms (ops, op_seconds)
        bb=b.get(k,{})
        d[k]={o:round(v[o]-bb.get(o,0),4) for o in v if v[o]-bb.get(o,0)}
    elif isinstance(v,(int,float)):
        d[k]=round(v-b.get(k,0),3)
print(json.dumps({"layout":sys.argv[1],"step":sys.argv[2],"compiled":int(sys.argv[3]),
  "real":float(real),"user":float(user),"sys":float(sys_),"upper_bytes":int(sys.argv[4]),**d}))
PY
  tail -1 "$RESULTS"
}

seq_full() { # clean, noop, leaf, core, noop on the current /t
  local L=$1
  reset_src
  step $L clean
  step $L noop
  edits $L
}
edits() { # three rounds of leaf + core edits, then a no-op
  local L=$1 i
  for i in 1 2 3; do
    edit $LEAF; step $L leaf
    edit $CORE; step $L core
  done
  step $L noop2
}
seq_restore() { # /t prefilled with A's final target (S1): restore, noop, leaf, core, noop
  local L=$1
  cp -p $W/s1/leaf $SRC/$LEAF; cp -p $W/s1/core $SRC/$CORE
  step $L restore
  step $L noop
  edits $L
}

start_fuse() { # MNT args...
  local mnt=$1; shift
  statsfile=/tmp/stats-$$.json
  $TIERFS "$@" -mnt $mnt -stats $statsfile > $W/logs/tierfs-$(date +%s).log 2>&1 &
  fusepid=$!
  for _ in $(seq 100); do grep -q " $mnt fuse" /proc/mounts && return 0; sleep 0.1; done
  echo "fuse mount failed"; cat $W/logs/tierfs-*.log | tail; exit 1
}
stop_fuse() { [ -n "$fusepid" ] && { kill -TERM "$fusepid"; wait "$fusepid" || true; fusepid=""; }; }
umount_t() { umount /t 2>/dev/null || true; }

layout_A() {
  local tag=${1:-A}
  umount_t; mkdir -p $RUNS/$tag; mount --bind $RUNS/$tag /t
  seq_full $tag
  [ "$tag" = A ] || { umount_t; return; }
  # A's final target matches the edited sources (S1); the restore layouts
  # start from exactly that state.
  mkdir -p $W/s1; cp -p $SRC/$LEAF $W/s1/leaf; cp -p $SRC/$CORE $W/s1/core
  umount_t
}

layout_C() { # target fully on the read-write FUSE; extra args e.g. -writeback
  # (the kernel refuses passthrough when writeback cache is negotiated)
  local tag=${1:-C} ; shift || true
  umount_t; mkdir -p $RUNS/$tag
  start_fuse /t rw -backing $RUNS/$tag "$@"
  seq_full $tag
  stop_fuse
}

pack_S0() { # lower store from A's final (S1) target (hardlinks, no extra disk)
  [ -f $RUNS/store/manifest.json ] || { mkdir -p $RUNS/store; $TIERFS pack -src $RUNS/A -store $RUNS/store -link | tee -a $W/logs/pack.log; }
}

mount_overlay() { # lowerdir upper-tag [extra opts]
  local lower=$1 tag=$2 extra=${3:-}
  mkdir -p $RUNS/$tag/upper $RUNS/$tag/work
  mount -t overlay overlay -o lowerdir=$lower,upperdir=$RUNS/$tag/upper,workdir=$RUNS/$tag/work$extra /t
}

layout_B() { # overlay: upper local, lower = RO FUSE over local chunk dir
  local tag=${1:-B}; shift || true
  pack_S0; umount_t; mkdir -p /mnt/lower
  start_fuse /mnt/lower ro -store $RUNS/store "$@"
  mount_overlay /mnt/lower $tag "${OVL_OPTS:-}"
  grep " /t overlay" /proc/mounts
  seq_restore $tag
  umount_t; stop_fuse
}

layout_Bclean() { # overlay with an empty FUSE lower: clean build cost through overlay
  local tag=${1:-Bclean}
  mkdir -p $RUNS/empty-src $RUNS/empty-store
  [ -f $RUNS/empty-store/manifest.json ] || $TIERFS pack -src $RUNS/empty-src -store $RUNS/empty-store
  umount_t; mkdir -p /mnt/lower
  start_fuse /mnt/lower ro -store $RUNS/empty-store
  mount_overlay /mnt/lower $tag
  reset_src; step $tag clean; step $tag noop
  umount_t; stop_fuse
}

layout_D() { # like B, lower lazily fetched from a slow "object store"; extra tierfs args
  local tag=${1:-D}; shift || true
  pack_S0; umount_t; mkdir -p /mnt/lower
  start_fuse /mnt/lower ro -store $RUNS/store -cache $RUNS/$tag-cache -latency 5ms -bw 200e6 -parallel 16 "$@"
  mount_overlay /mnt/lower $tag "${OVL_OPTS:-}"
  grep " /t overlay" /proc/mounts
  seq_restore $tag
  umount_t; stop_fuse
}

# Layouts E and F use the real implementation (internal/anyfs via
# anyfsbench): pages + a base layer in a directory store, local tier and
# materialized files in a cache dir, read-only FUSE lower, kernel overlay.
pack_anyfs() {
  [ -f $RUNS/astore/heads/bench ] || { mkdir -p $RUNS/astore; $ANYFS pack -src $RUNS/A -store $RUNS/astore | tee -a $W/logs/anyfs-pack.log; }
}
start_anyfs() { # MNT args...
  local mnt=$1; shift
  statsfile=/tmp/stats-$$${mnt//\//_}.json
  $ANYFS mount "$@" -mnt $mnt -stats $statsfile > $W/logs/anyfs-$(date +%s).log 2>&1 &
  fusepid=$!
  for _ in $(seq 600); do grep -q " $mnt fuse" /proc/mounts && return 0; sleep 0.1; done
  echo "anyfs mount failed"; tail $W/logs/anyfs-*.log; exit 1
}
commit_step() { # commit the merged /t as a layer (writeback cost)
  $ANYFS commit -store $RUNS/astore -cache $RUNS/$1-cache -dir /t -to bench-$1 | python3 -c "import json,sys; d=json.load(sys.stdin); d['layout']='$1'; d['step']='commit'; print(json.dumps(d))" | tee -a "$RESULTS"
}
layout_E() { # anyfs, warm slot: every file materialized locally before the mount
  local tag=${1:-E}; shift || true
  pack_anyfs; umount_t; mkdir -p /mnt/lower
  start_anyfs /mnt/lower -store $RUNS/astore -cache $RUNS/$tag-cache -warm "$@"
  mount_overlay /mnt/lower $tag ",redirect_dir=on,metacopy=on"
  seq_restore $tag
  commit_step $tag
  umount_t; stop_fuse
}
layout_F() { # anyfs, cold restore from a slow store (5 ms/object, 200 MB/s), prefetch <= 64 KiB
  local tag=${1:-F}; shift || true
  pack_anyfs; umount_t; mkdir -p /mnt/lower
  start_anyfs /mnt/lower -store $RUNS/astore -cache $RUNS/$tag-cache -latency 5ms -bw 200e6 -prefetch 65536 "$@"
  mount_overlay /mnt/lower $tag ",redirect_dir=on,metacopy=on"
  seq_restore $tag
  commit_step $tag
  umount_t; stop_fuse
}

# Alternating comparison in one window: every variant stays mounted at
# /mnt/v/TAG for the whole run (its caches stay warm); each step
# bind-mounts one variant at /t, so all variants build the same source
# states back to back (order alternates per round).
#   layout_alt ROUNDS TAG...   e.g. layout_alt 5 A E
# Tags: A = plain directory (A's target, continuing from S1), E = anyfs
# (FUSE lower + overlay, warm slot), M = anyfs with the metadata layer
# (no FUSE in the warm path).
declare -A fpids sfiles
alt_up() { # TAG
  local tag=$1; mkdir -p /mnt/v/$tag
  case $tag in
    A) mount --bind $RUNS/A /mnt/v/A ;;
    E*|M*)
      local extra=(); [[ $tag == M* ]] && extra=(-meta $RUNS/$tag-meta)
      mkdir -p /mnt/lower-$tag
      start_anyfs /mnt/lower-$tag -store $RUNS/astore -cache $RUNS/$tag-cache -warm "${extra[@]}" ${ALT_ARGS:-}
      fpids[$tag]=$fusepid; sfiles[$tag]=$statsfile
      local lower=/mnt/lower-$tag
      [[ $tag == M* ]] && lower=$(cat $RUNS/$tag-meta.lowerdir)
      mkdir -p $RUNS/$tag/upper $RUNS/$tag/work
      mount -t overlay overlay -o lowerdir=$lower,upperdir=$RUNS/$tag/upper,workdir=$RUNS/$tag/work,redirect_dir=on,metacopy=on /mnt/v/$tag ;;
    *) echo "unknown alt tag $tag"; exit 1 ;;
  esac
}
alt_down() { # TAG
  local tag=$1
  umount /mnt/v/$tag || umount -l /mnt/v/$tag
  if [ -n "${fpids[$tag]:-}" ]; then fusepid=${fpids[$tag]}; stop_fuse; fi
}
alt_step() { # TAG NAME
  local tag=$1
  fusepid=${fpids[$tag]:-}; statsfile=${sfiles[$tag]:-}
  umount_t; mount --bind /mnt/v/$tag /t
  step "$tag" "$2"
  umount_t
}
alt_all() { # NAME ROUND TAG...: one step per tag, order reversed on odd rounds
  local name=$1 r=$2; shift 2
  local tags=("$@") i
  if (( r % 2 )); then for (( i=${#tags[@]}-1; i>=0; i-- )); do alt_step ${tags[i]} $name; done
  else for i in "${tags[@]}"; do alt_step $i $name; done; fi
}
layout_alt() {
  local rounds=$1; shift
  pack_anyfs; umount_t
  cp -p $W/s1/leaf $SRC/$LEAF; cp -p $W/s1/core $SRC/$CORE
  local t r
  for t in "$@"; do alt_up $t; done
  alt_all restore 0 "$@"
  alt_all noop 1 "$@"
  for (( r=0; r<rounds; r++ )); do
    edit $LEAF; alt_all leaf $r "$@"
    edit $CORE; alt_all core $((r+1)) "$@"
  done
  alt_all noop2 0 "$@"
  for t in "$@"; do
    [ "$t" = A ] || [ -z "${ALT_COMMIT:-}" ] && continue
    fusepid=${fpids[$t]}; statsfile=${sfiles[$t]}
    umount_t; mount --bind /mnt/v/$t /t; commit_step $t; umount_t
  done
  for t in "$@"; do alt_down $t; done
}

layout_freshclone() { # restore onto a source tree whose mtimes are "now" (git clone)
  pack_S0; umount_t; mkdir -p /mnt/lower
  start_fuse /mnt/lower ro -store $RUNS/store
  mount_overlay /mnt/lower fresh ",redirect_dir=on,metacopy=on"
  cp -p $W/s1/leaf $SRC/$LEAF; cp -p $W/s1/core $SRC/$CORE
  step fresh restore
  find $SRC -path $SRC/site -prune -o -name '*.rs' -print0 | xargs -0 touch
  step fresh touched-all
  umount_t; stop_fuse
}

"$@"
