#!/bin/bash
# Builds the headstart toolchain (github.com/PowderworksCode/headstart: rustc
# stage1 sysroot with -Zearly-metadata plus cargo with -Zheadstart) and packs
# it as /out/headstart-<rev>-x86_64-unknown-linux-gnu.tar.gz for
# [environment.toolchains] extra = ["headstart"] (docs/rust.md).
#
# Run in debian:bookworm (glibc 2.36, so the result runs on bookworm and
# newer images) on a large x86_64 machine, about 20 minutes on 16 vCPU:
#
#   docker run --rm -v /work:/work -v /out:/out -v $PWD/build.sh:/build.sh \
#     debian:bookworm bash /build.sh
#
# Then upload it to s3://<artifact bucket>/toolchains/ and update
# headstartVersion and the checksum in internal/envbuild/tools.go.
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends git python3 curl ca-certificates build-essential pkg-config libssl-dev xz-utils pigz cmake >/dev/null
git config --global --add safe.directory "*"
curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain 1.98.1
. /root/.cargo/env
cd /work
[ -d headstart ] || git clone -q https://github.com/PowderworksCode/headstart
cd headstart
REV=$(git rev-parse --short=10 HEAD)
git submodule update --init --depth 1 rustc cargo
for p in patches/rustc/*.patch; do git -C rustc apply "$PWD/$p"; done
for p in patches/cargo/*.patch; do git -C cargo apply "$PWD/$p"; done
cp config/bootstrap.toml rustc/bootstrap.toml
# Distribution build: no incremental rustc, jemalloc like rustup's builds.
(cd rustc && ./x build --stage 1 compiler library --set rust.incremental=false --set rust.jemalloc=true)
(cd cargo && cargo +1.98.1 build --release)
STAGE=/work/pack/headstart-$REV
rm -rf /work/pack && mkdir -p "$STAGE"
tar -C rustc/build/host/stage1 --exclude=./lib/rustlib/src --exclude=./lib/rustlib/rustc-src -chf - . | tar -C "$STAGE" -xf -
find "$STAGE" -type l
install -m755 cargo/target/release/cargo "$STAGE/bin/cargo"
printf 'headstart %s\nrustc %s\ncargo %s\n' "$REV" "$(git -C rustc rev-parse HEAD)" "$(git -C cargo rev-parse HEAD)" > "$STAGE/HEADSTART-VERSION"
"$STAGE/bin/rustc" -vV
"$STAGE/bin/cargo" -V
ls "$STAGE/bin"
tar -C /work/pack -cf - "headstart-$REV" | pigz -6 > "/out/headstart-$REV-x86_64-unknown-linux-gnu.tar.gz"
cd /out && sha256sum *.tar.gz && ls -la && du -sh "$STAGE"
