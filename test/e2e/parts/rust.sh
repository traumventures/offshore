#!/usr/bin/env bash
# rust: cargo through the proxy: builds, eager and lazy outputs, the output
# FUSE, rust-analyzer (embedded mode only), the crates.io registry cache,
# sccache and mold.
PART=rust
. "$(dirname "$0")/../lib.sh"

step "first remote build (environment image, machine provisioning, FUSE, outputs)"
out="$(run cargo build 2>&1)"; code=$?
check "cargo build exits 0" eq "$code" 0
ready
check "compiled at the local path" contains "$out" "Compiling hello v0.1.0 ($PROJ)"
check "binary transferred (eager output)" test -x "$PROJ/target/debug/hello"
check "binary runs locally" contains "$("$PROJ/target/debug/hello")" "greeting=hello ran=local"
check "Cargo.lock written back" test -f "$PROJ/Cargo.lock"
MACHINE="$(machines | head -1)"

step "incremental build and cargo run"
sed -i 's/greeting=hello/greeting=changed/' "$PROJ/src/main.rs"
out="$(run cargo run 2>&1)"
check "edit picked up immediately" contains "$out" "greeting=changed ran=remote"
check "new binary transferred" contains "$("$PROJ/target/debug/hello")" "greeting=changed"
out="$(run cargo build 2>&1)"
check "no-op build is fresh (warm build cache)" eq "$(echo "$out" | grep -c Compiling)" 0
check "worker-local build directory is mounted remotely" eq "$(run sh -c 'ls target | grep -c debug')" "1"

step "compiler errors and exit codes"
printf 'pub fn f() { let _x: u32 = "a"; }\n' > "$PROJ/src/broken.rs"
cp "$PROJ/src/main.rs" "$WORK/main.rs.bak"; echo 'mod broken;' >> "$PROJ/src/main.rs"
out="$(run cargo build 2>&1)"; code=$?
check "failing build returns cargo's exit code" eq "$code" 101
check "compiler errors are streamed" contains "$out" "expected \`u32\`, found \`&str\`"
cp "$WORK/main.rs.bak" "$PROJ/src/main.rs"; rm "$PROJ/src/broken.rs"

step "cargo test: remote build, local test run"
out="$(run cargo test -- --nocapture 2>&1)"; code=$?
check "cargo test exits 0" eq "$code" 0
check "test binary built remotely, run locally" contains "$out" "test ran=local manifest_dir=$PROJ"
check "cargo's status line for the local run" contains "$out" "Running unittests src/main.rs (target/debug/deps/hello-"
check "no build-phase artifacts in the output" sh -c "! echo \"\$1\" | grep -q -e Executable -e '\"reason\"'" _ "$out"
out="$(E2E_TEST_FAIL=1 run cargo test 2>&1)"; code=$?
check "failing local test gives cargo's exit code" eq "$code" 101
check "cargo's rerun hint" contains "$out" "error: test failed, to rerun pass \`--bin hello\`"
out="$(CLOUDSOURCED_SPLIT=0 run cargo test -- --nocapture 2>&1)"
check "CLOUDSOURCED_SPLIT=0 runs the tests remotely" contains "$out" "test ran=remote"

step "shims (transparent proxy through PATH)"
cs shim install cargo >/dev/null
out="$(cd "$PROJ" && PATH="$CLOUDSOURCED_SHIM_DIR:$PATH" cargo run -q 2>&1)"
check "cargo via shim runs remotely" contains "$out" "ran=remote"

step "editor integration (rust-analyzer flycheck through the proxy)"
out="$(cd "$PROJ" && "$BIN" run cargo check --workspace --message-format=json 2>/dev/null)"
check "JSON diagnostics on stdout with local paths" contains "$(echo "$out" | head -1)" "\"reason\":\"compiler-artifact\""
check "check reports the local manifest path" contains "$out" "\"manifest_path\":\"$PROJ/Cargo.toml\""

step "lazy outputs"
rm -f "$PROJ/target/debug/hello.d"
out="$(cd "$PROJ" && cs fetch 'target/debug/*.d' 2>&1)"
check "fetch copies outputs from the latest build" test -f "$PROJ/target/debug/hello.d"

step "output FUSE (remote target/ mounted read-only, content on demand)"
out="$(cd "$PROJ" && cs outputs mount 2>&1)"
check "outputs mounted at the local build directory" contains "$out" "mounted $PROJ/target"
check "mount is a read-only FUSE file system" contains "$(grep " $PROJ/target " /proc/self/mountinfo)" "ro,"
remote_ls="$(run sh -c 'ls target/debug/deps | sort')"
check "remote directory listing visible locally" eq "$(ls "$PROJ/target/debug/deps" | sort)" "$remote_ls"
bin="$(cd "$PROJ" && ls target/debug/deps/hello-* | grep -v '\.d$' | head -1)"
check "non-eager file is read on demand" eq "$(sha256sum < "$PROJ/$bin")" "$(run sh -c "sha256sum < $bin")"
check "local files below the mount stay visible" test -f "$PROJ/target/debug/hello.d"
check "writes into the mount are rejected (local cargo cannot write)" sh -c "! touch '$PROJ/target/debug/x' 2>/dev/null"
sed -i 's/greeting=changed/greeting=fuse/' "$PROJ/src/main.rs"
run cargo build -q >/dev/null 2>&1
check "new run invalidates the mount (eager output from the content cache)" contains "$("$PROJ/target/debug/hello")" "greeting=fuse"
check "outputs status shows the mount" contains "$(cs outputs status)" "mounted at $PROJ/target"
json="$(run cargo check --workspace --message-format=json 2>/dev/null)"
check "cargo check artifact paths exist and are readable through the mount" eq "$(echo "$json" | python3 -c '
import json, sys
n = bad = 0
for line in sys.stdin:
    m = json.loads(line)
    if m.get("reason") != "compiler-artifact":
        continue
    for f in m["filenames"]:
        n += 1
        try:
            open(f, "rb").read(1)
        except OSError as e:
            bad += 1
            print(f, e, file=sys.stderr)
print("ok" if n and not bad else "%d of %d unreadable" % (bad, n))')" ok
so="$(echo "$json" | python3 -c '
import json, sys
for line in sys.stdin:
    m = json.loads(line)
    if m.get("reason") == "compiler-artifact" and "proc-macro" in m["target"]["kind"]:
        print(m["filenames"][0])')"
check "proc-macro library is an ELF shared object below the mount" contains "$(file -L "$so")" "ELF 64-bit LSB shared object"
check "proc-macro library loads locally (dlopen)" python3 -c 'import ctypes, sys; ctypes.CDLL(sys.argv[1])' "$so"
out="$(cd "$PROJ" && cs editor --mode lsp-remote vscode 2>&1)"
check "editor lsp-remote routes checks through the proxy" contains "$out" '"rust-analyzer.cargo.buildScripts.overrideCommand": ["'
if [ -n "$REMOTE" ]; then
  echo "     (rust-analyzer runs in the embedded mode only: skipped)"
elif RA="$(cd "$PROJ" && rustup which rust-analyzer 2>/dev/null)" && [ -x "$RA" ]; then
  # rust-analyzer runs locally with the lsp-remote settings: checks and build
  # scripts go through the proxy, the proc macro built remotely is loaded
  # from the mount. The type of Greeter::NAME.len() needs the expanded derive.
  out="$(python3 "$HERE/ra_lsp.py" "$RA" "$PROJ" "[\"$BIN\",\"run\",\"cargo\",\"check\",\"--workspace\",\"--all-targets\",\"--message-format=json\"]" 2>&1)"
  check "rust-analyzer (LSP, lsp-remote settings) expands the remote proc macro" contains "$out" "let _len: usize"
  # Its remote checks end with it; later steps expect the repository's
  # build slot to be free (a busy slot gives a session another one).
  poll 60 no_sessions >/dev/null
else
  echo "     (rust-analyzer for the remote toolchain not installed locally: skipped)"
fi

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
check "why shows the run's sccache counters" contains "$(cs why)" "sccache_hits="
check "compiler cache entries on the shared dependency volume" test "$(docker_ exec "$MACHINE" sh -c 'find /deps/sccache -type f | wc -l')" -ge 1


step "operations: compiler cache and output metrics"
# Machine metrics are those of the last probe: wait for one after the run.
check "metrics: sccache hits republished from the machine" poll 30 metric_ge cloudsourced_worker_compiler_cache_total 'counter="sccache_hits"' 1
check "metrics: output bytes transferred" poll 30 metric_ge cloudsourced_worker_transfer_bytes_total 'direction="output"' 1
cs outputs unmount >/dev/null 2>&1 || true
