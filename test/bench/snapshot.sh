#!/usr/bin/env bash
# Benchmark of build slot snapshots: how long a new machine needs to get a
# warm target/ back, by snapshot restore versus a rebuild with a warm
# sccache (the global result cache), on a small crate with registry
# dependencies, build scripts and a proc macro.
#
# Runs on this host with the local toolchain and a directory store (the
# local-docker native store is the same blobcache.DirStore), so it needs
# neither docker nor a cloud account; set BENCH_STORE to a mounted bucket
# (s3fs, rclone) to include network transfer. Steps:
#   1. cold build with sccache (fills the result cache)
#   2. snapshot save of target/ (and a second, incremental save)
#   3. lazy restore into a fresh target/: skeleton, first file, full fill;
#      then `cargo build` must find every unit fresh (mtimes restored)
#   4. rebuild from scratch with the warm sccache
#
#   test/bench/snapshot.sh            # prints a table, keeps nothing
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d /tmp/cs-snapbench.XXXXXX)"
cleanup() { python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1], ignore_errors=True)' "$WORK"; }
trap cleanup EXIT
STORE="${BENCH_STORE:-$WORK/store}"
export SCCACHE_DIR="$WORK/sccache" SCCACHE_IDLE_TIMEOUT=0 CARGO_INCREMENTAL=0
export RUSTC_WRAPPER="${RUSTC_WRAPPER:-sccache}"
export SCCACHE_SERVER_PORT=$((20000 + RANDOM % 20000))

(cd "$ROOT" && go build -o "$WORK/snapshotbench" ./test/bench/snapshotbench)

mkdir -p "$WORK/crate/src"
cat > "$WORK/crate/Cargo.toml" <<'TOML'
[package]
name = "bench"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
itoa = "1"
libc = "0.2"
TOML
cat > "$WORK/crate/src/main.rs" <<'RS'
#[derive(serde::Serialize)]
struct P { n: String }
fn main() { println!("{}", serde_json::to_string(&P { n: itoa::Buffer::new().format(unsafe { libc::abs(-42) }).to_string() }).unwrap()); }
RS
cd "$WORK/crate"
now() { date +%s.%N; }
since() { python3 -c "import sys; print(f'{float(sys.argv[2])-float(sys.argv[1]):.2f}')" "$1" "$(now)"; }
fresh_units() { cargo build --message-format=json 2>/dev/null | python3 -c '
import json, sys
print(sum(1 for l in sys.stdin if l.startswith("{") and json.loads(l).get("reason") == "compiler-artifact" and not json.loads(l).get("fresh")))'; }

cargo fetch -q
sccache --stop-server >/dev/null 2>&1 || true
t=$(now); cargo build -q; cold=$(since "$t")

save1=$("$WORK/snapshotbench" save target "$STORE")
touch src/main.rs && cargo build -q
save2=$("$WORK/snapshotbench" save target "$STORE")

mv target target.orig
first=$(cd target.orig && ls debug/deps/libserde-*.rlib | head -1)
restore=$("$WORK/snapshotbench" restore "$STORE" "$WORK/crate/target" "debug/deps/$(basename "$first")")
recompiled=$(fresh_units)

python3 -c 'import shutil; shutil.rmtree("target")'
t=$(now); cargo build -q; rebuild=$(since "$t")
sccache --stop-server >/dev/null 2>&1 || true

python3 - "$cold" "$rebuild" "$recompiled" "$save1" "$save2" "$restore" <<'PY'
import json, sys
cold, rebuild, recompiled = sys.argv[1], sys.argv[2], sys.argv[3]
s1, s2, r = (json.loads(x) for x in sys.argv[4:7])
print(f"target/ size                 {s1['bytes']/2**20:8.1f} MB in {s1['files']} files, {s1['chunks']} chunks")
print(f"cold build (sccache filling) {float(cold):8.2f} s")
print(f"snapshot save (first)        {s1['seconds']:8.2f} s  ({s1['new_chunks']} chunks uploaded)")
print(f"snapshot save (incremental)  {s2['seconds']:8.2f} s  ({s2['new_chunks']} chunks uploaded)")
print(f"restore: skeleton            {r['skeleton_seconds']:8.2f} s")
print(f"restore: first file          {r.get('first_file_seconds', 0):8.2f} s")
print(f"restore: full fill           {r['fill_seconds']:8.2f} s  ({r['chunks_fetched']} chunks)")
print(f"units rebuilt after restore  {int(recompiled):8d}    (0 = mtimes and content intact)")
print(f"rebuild with warm sccache    {float(rebuild):8.2f} s")
PY
