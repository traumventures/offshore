#!/usr/bin/env bash
# Benchmark of the output overlay ([outputs] mode = "fuse"): build a small
# crate (workspace crate, proc macro, registry dependencies with build
# scripts, a test) remotely with the local-docker backend, then run cargo
# LOCALLY against the mounted build directory and count the units cargo
# recompiles. Goal: 0 for unchanged sources.
#
# Not part of the e2e suite. Uses its setup (test/e2e/lib.sh): requires
# docker, /dev/fuse, bin/offshore, and a local rustup toolchain of the
# version pinned below (default: the local `rustc` version).
#
#   E2E_DOCKER="sudo -n docker" test/bench/overlay-fresh.sh
#   BENCH_CARGO_HOME=/tmp/clean-cargo-home  # local CARGO_HOME (default: yours)
PART=overlay-bench
export E2E_RUSTV="${E2E_RUSTV:-$(rustc --version | awk '{print $2}')}"
. "$(dirname "$0")/../e2e/lib.sh"
[ -n "${BENCH_CARGO_HOME:-}" ] && export CARGO_HOME="$BENCH_CARGO_HOME"

cat >> "$PROJ/.offshore.toml" <<'TOML'

[outputs]
mode = "fuse"
TOML
cat >> "$PROJ/Cargo.toml" <<'TOML'
itoa = "1"
libc = "0.2"
TOML
cat >> "$PROJ/src/main.rs" <<'RS'

pub fn digits(n: i32) -> String { itoa::Buffer::new().format(n).to_string() }

#[cfg(test)]
mod tests {
    #[test]
    fn digits() { assert_eq!(super::digits(unsafe { libc::abs(-42) }), "42"); }
}
RS

# local CMD...: cargo on this machine (not the proxy), in the project.
lc() { (cd "$PROJ" && env -u OFFSHORE_STATE_DIR -u OFFSHORE_CONFIG cargo "$@"); }
# recompiled JSON: units cargo did not find fresh ("fresh":false artifacts
# and executed build scripts), with their names on stderr.
recompiled() {
  python3 -c '
import json, sys
n = 0
for line in sys.stdin:
    try: m = json.loads(line)
    except ValueError: continue
    if m.get("reason") == "compiler-artifact" and not m.get("fresh"):
        n += 1; print("     recompiled:", m["target"]["name"], m["target"]["kind"], file=sys.stderr)
    if m.get("reason") == "build-script-executed" and not m.get("fresh", True):
        n += 1
print(n)'
}
units() { python3 -c '
import json, sys
print(sum(1 for l in sys.stdin if l.startswith("{") and json.loads(l).get("reason") == "compiler-artifact"))'; }

step "remote build (cargo build, cargo test --no-run)"
out="$(run cargo build 2>&1)"; code=$?
check "remote cargo build exits 0" eq "$code" 0
[ "$code" = 0 ] || echo "$out" | tail -20
out="$(run cargo test --no-run 2>&1)"; code=$?
check "remote cargo test --no-run exits 0" eq "$code" 0
check "overlay mounted at target/" contains "$(grep " $PROJ/target " /proc/self/mountinfo)" "fuse.offshore-outputs"
check "remote session saw the client's CARGO_HOME" eq "$(run sh -c 'echo $CARGO_HOME')" "${CARGO_HOME:-$HOME/.cargo}"

step "local cargo against the overlay"
t=$SECONDS; json="$(lc build --message-format=json 2>"$WORK/b.err")"; code=$?
n="$(echo "$json" | recompiled)"; total="$(echo "$json" | units)"
echo "     local cargo build: $n of $total units recompiled ($((SECONDS - t))s)"
[ "$code" = 0 ] || tail -20 "$WORK/b.err"
check "local cargo build recompiles nothing" eq "$n" 0
t=$SECONDS; json="$(lc test --no-run --message-format=json 2>"$WORK/t.err")"
n="$(echo "$json" | recompiled)"; total="$(echo "$json" | units)"
echo "     local cargo test --no-run: $n of $total units recompiled ($((SECONDS - t))s)"
check "local cargo test --no-run recompiles nothing" eq "$n" 0
out="$(lc test -q 2>&1)"
check "remote-built test binary runs locally" contains "$out" "1 passed"
check "remote-built binary runs locally (cargo run)" contains "$(lc run -q 2>&1)" "ran=local"
echo "     verbose:"; lc build -v 2>&1 | grep -E "Fresh|Compiling|Dirty" | sed 's/^/       /'

step "edit, remote build, local cargo again"
sed -i 's/greeting=hello/greeting=edited/' "$PROJ/src/main.rs"
out="$(run cargo build 2>&1)"
check "remote rebuild compiles the edit" contains "$out" "Compiling hello"
json="$(lc build --message-format=json 2>/dev/null)"
n="$(echo "$json" | recompiled)"
echo "     local cargo build after remote rebuild: $n recompiled"
check "overlay follows the new remote build (nothing to recompile)" eq "$n" 0
check "new binary runs locally" contains "$(lc run -q 2>&1)" "greeting=edited ran=local"

step "local edit: only the changed crate compiles locally, into the upper layer"
sed -i 's/greeting=edited/greeting=local-edit/' "$PROJ/src/main.rs"
json="$(lc build --message-format=json 2>/dev/null)"
check "one unit recompiled locally" eq "$(echo "$json" | recompiled 2>/dev/null)" 1
check "local build result runs" contains "$(lc run -q 2>&1)" "greeting=local-edit"
upper="$(cs outputs status | sed -n 's/.*local writes in \([^)]*\)).*/\1/p' | head -1)"
check "local writes are in the upper layer (client state)" test -d "$upper/debug"
(cd "$PROJ" && cs outputs unmount >/dev/null)
check "unmount removes the upper layer" test ! -e "$upper"
