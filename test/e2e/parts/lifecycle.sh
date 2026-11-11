#!/usr/bin/env bash
# lifecycle: idle stop, restart on demand with warm caches, lifetime
# expiry and volume cleanup, the compiler cache across machines, fail open.
# Time moves with the test clock (offshore debug clock advance), not
# by waiting: idle_stop 5m and max_lifetime 24h take seconds.
PART=lifecycle
. "$(dirname "$0")/../lib.sh"

# A crates.io dependency without the network: itoa from a vendored
# directory source (like a registry crate, sccache can cache it; path
# dependencies are compiled incrementally and could not be cached).
mkdir -p "$PROJ/vendor" "$PROJ/.cargo"
cp -r "$HERE/vendor/itoa" "$PROJ/vendor/"
cat > "$PROJ/.cargo/config.toml" <<'TOML'
[source.crates-io]
replace-with = "vendored-sources"

[source.vendored-sources]
directory = "vendor"
TOML
cat >> "$PROJ/Cargo.toml" <<'TOML'
itoa = "1"
TOML
sed -i 's/fn main() {/fn main() {\n    let mut b = itoa::Buffer::new(); println!("itoa={}", b.format(42));/' "$PROJ/src/main.rs"

step "first build with a vendored dependency, outputs mounted"
out="$(run cargo run -q 2>&1)"
check "vendored crate built and used remotely" contains "$out" "itoa=42"
ready
out="$(cd "$PROJ" && cs outputs mount 2>&1)"
check "outputs mounted at the local build directory" contains "$out" "mounted $PROJ/target"

step "lifecycle: idle stop, warm restart, lifetime expiry, cleanup"
cs down >/dev/null
write_config 24h 5m 0 2
# Pool settings change by explicit reload, without a restart.
cs reconcile >/dev/null 2>&1; code=$?
check "reconcile reloads the pool configuration" eq "$code" 0
run cargo build -q >/dev/null 2>&1
check "output mount restored after the daemon restart" grep -q " $PROJ/target .*fuse.offshore-outputs" /proc/self/mountinfo
cached="$(cd "$PROJ" && ls target/debug/deps/hello-*.d | head -1)"; cached_sum="$(sha256sum < "$PROJ/$cached")"
M="$(machines | head -1)"
NAME="$(docker_ inspect -f '{{.Name}}' "$M" | tr -d /)"
status_of() { docker_ inspect -f '{{.State.Status}}' "$NAME" 2>/dev/null; }
check "machine still running before idle_stop" eq "$(status_of)" running
advance 6m
poll 60 sh -c '[ "$('"$DOCKER"' inspect -f "{{.State.Status}}" "$1")" = exited ]' _ "$NAME" >/dev/null
check "idle machine stopped after idle_stop" eq "$(status_of)" exited
check "stopped machine keeps its cache volume" test -n "$(docker_ volume ls -q --filter "name=cs-cache-${NAME#cs-}")"
check "output FUSE serves cached content after the machine stopped" eq "$(sha256sum < "$PROJ/$cached")" "$cached_sum"
t0=$SECONDS
check "uncached outputs of a stopped machine fail (EIO), they do not hang" sh -c "! timeout 60 cat '$PROJ/${cached%.d}' >/dev/null 2>&1"
check "  ... and fail fast" test $((SECONDS - t0)) -lt 30
out="$(OFFSHORE_VERBOSE=1 run cargo build 2>&1)"
check "stopped machine restarted on demand" contains "$out" "starting machine ${NAME#cs-}"
check "restarted machine still has a warm build cache" eq "$(echo "$out" | grep -c Compiling)" 0
check "services restart with their machine" contains "$(http_service)" "200 OK"
check "machine not destroyed before max_lifetime" test -n "$(docker_ ps -aq --filter "name=^$NAME\$")"
advance 25h
poll 60 sh -c '[ -z "$('"$DOCKER"' ps -aq --filter "name=^$1\$")" ]' _ "$NAME" >/dev/null
check "machine destroyed after max_lifetime" test -z "$(docker_ ps -aq --filter "name=^$NAME\$")"
poll 30 sh -c '[ -z "$('"$DOCKER"' volume ls -q --filter "name=cs-cache-$1")" ]' _ "${NAME#cs-}" >/dev/null
check "its build cache volume is gone" test -z "$(docker_ volume ls -q --filter "name=cs-cache-${NAME#cs-}")"
# The orchestrator saved the build slot before destroying the machine; the
# fresh machine restores it (the slot is younger than the build TTL on the
# worker's real clock; only the orchestrator's test clock moved 25h).
out="$(OFFSHORE_VERBOSE=1 run cargo build 2>&1)"
check "fresh machine restores the build slot snapshot" contains "$out" "(restore)"
check "restored build slot is warm" eq "$(echo "$out" | grep -c Compiling)" 0
run cargo clean -q -p itoa >/dev/null 2>&1
out="$(OFFSHORE_VERBOSE=1 run cargo build 2>&1)"
check "fresh machine recompiles a cleaned dependency" contains "$out" "Compiling itoa"
check "fresh machine gets compiler cache hits for dependencies" test "$(sccache_hits "$out")" -ge 1

out="$(cd "$PROJ" && cs outputs unmount 2>&1)"
check "outputs unmount" test -z "$(grep " $PROJ/target " /proc/self/mountinfo)"
check "local build directory intact after unmount" test -x "$PROJ/target/debug/hello"

step "fail open when no machine can be provisioned"
write_config 24h 10m 0 2
sed -i 's/^command = .*/command = ["false"]/' "$POOLCFG"; restart_daemon
out="$(cd "$PROJ" && OFFSHORE_REQUIRE_REMOTE= "$BIN" run sh -c 'echo ${OFFSHORE_SESSION:-local}' 2>&1)"
check "falls back to local execution" contains "$out" "local"
(cd "$PROJ" && "$BIN" run sh -c true >/dev/null 2>&1); code=$?
check "OFFSHORE_REQUIRE_REMOTE turns fallback into an error" eq "$code" 125
write_config 24h 10m 0 2; restart_daemon
