#!/usr/bin/env bash
# Correctness checks, run INSIDE the bench container after bench.sh pack_S0:
# overlay over the read-only tierfs lower (default options and
# redirect_dir+metacopy), and the read-write tierfs.
set -uo pipefail
W=/work RUNS=/work/runs TIERFS=/work/tierfs
export CARGO_TARGET_DIR=/t CARGO_INCREMENTAL=1 CARGO_TERM_COLOR=never
cp -p $W/s1/leaf $W/src/cargo-nextest/src/main.rs; cp -p $W/s1/core $W/src/nextest-metadata/src/lib.rs
mkdir -p /mnt/lower /t
$TIERFS ro -store $RUNS/store -mnt /mnt/lower > /tmp/chk-ro.log 2>&1 & ro=$!
sleep 1
for opts in "" ",redirect_dir=on,metacopy=on"; do
  tag=chk$(echo "$opts" | tr -dc a-z | head -c 12)
  mkdir -p $RUNS/$tag/upper $RUNS/$tag/work
  mount -t overlay overlay -o lowerdir=/mnt/lower,upperdir=$RUNS/$tag/upper,workdir=$RUNS/$tag/work$opts /t
  echo "== overlay opts='${opts#,}'"
  (cd $W/src && cargo build -p cargo-nextest --locked --offline -j 8 2>&1 | grep -c '^ *Compiling' | sed 's/^/restore build recompiles: /')
  (cd $W/src && cargo build -p cargo-nextest --locked --offline -j 8 2>&1 | grep -c '^ *Compiling' | sed 's/^/second no-op recompiles: /')
  echo "uplift hardlink (inode nlink path):"; stat -c '  %i %h %n' /t/debug/cargo-nextest $(ls /t/debug/deps/cargo_nextest-* | grep -v '\.d$')
  python3 - $RUNS/store/manifest.json $RUNS/$tag/upper <<'PY'
import json,os,sys
m=json.load(open(sys.argv[1]))['entries']; up=sys.argv[2]
bad=same=cu=cubad=0; ex=[]
for e in m:
    if e['t'] not in ('f', ord('f')): continue
    p='/t/'+e['p']
    try: st=os.lstat(p)
    except FileNotFoundError: continue
    if st.st_mtime_ns==e['mt']: same+=1
    else:
        bad+=1
        if len(ex)<3: ex.append(e['p'])
    if os.path.lexists(os.path.join(up,e['p'])):
        cu+=1
        if st.st_mtime_ns!=e['mt']: cubad+=1
print(f"mtimes: {same} equal to lower manifest, {bad} differ (rewritten by cargo) e.g. {ex}")
print(f"copied-up/rewritten files in upper: {cu}, of which mtime differs: {cubad}")
PY
  d=$(ls -d /t/debug/build/*/ | head -1); d=${d%/}
  python3 - "$d" <<'PY'
import os,sys,errno
d=sys.argv[1]
try:
    os.rename(d,d+'-renamed'); os.rename(d+'-renamed',d); print("rename of lower-origin dir: OK")
except OSError as e: print("rename of lower-origin dir:", errno.errorcode[e.errno])
PY
  echo "upper usage: $(du -sh $RUNS/$tag/upper | cut -f1)"
  umount /t
done
kill $ro; wait $ro
echo "== read-write tierfs over A's target"
mkdir -p /mnt/rw; $TIERFS rw -backing $RUNS/A -mnt /mnt/rw > /tmp/chk-rw.log 2>&1 & rw=$!; sleep 1
stat -c '  %i %h %n' /mnt/rw/debug/cargo-nextest $(ls /mnt/rw/debug/deps/cargo_nextest-* | grep -v '\.d$')
stat -c '  mtime %Y.%y %n' /mnt/rw/debug/cargo-nextest $RUNS/A/debug/cargo-nextest
kill $rw; wait $rw
for t in chk chkredirectdiro; do python3 -c "import shutil;shutil.rmtree('$RUNS/$t',ignore_errors=True)"; done
