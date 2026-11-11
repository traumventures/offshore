# Guide: Rust

Cargo is the reference adapter: builds, checks and clippy run remotely,
tests are built remotely and run locally, and the build directory stays
warm on the machine.

## Prerequisites

* A configured pool: local Docker ([README](../../README.md#quick-start-local-docker-no-cloud-account)),
  [AWS](aws.md) or [OCI](oci.md).
* Shims: `offshore shim install cargo` and `~/.offshore/bin` first in
  `PATH` (`offshore shim status`).
* The same toolchain locally as remotely (a `rust-toolchain.toml`, used by
  rustup on both sides) when you run tests locally or use the output
  overlay.
* `cargo-nextest` locally if you use `cargo nextest run`.

## Setup

1. In the workspace (or the repository containing it), run
   `offshore init`. It detects the workspace (also in a subdirectory such
   as `app/`), the toolchain file, the CI image, apt packages, services,
   install steps and git dependencies, and prints a commented
   configuration. Review it and write it with `offshore init --write`.
2. Adjust resources: `cpu` is the initial estimate per cargo session (the
   history estimator learns the real usage per subcommand), `memory` the
   initial reservation.
3. `offshore doctor`, then `cargo build`; `offshore why` shows where it ran.

## Example configuration

Modelled on a production workspace (a Cargo workspace in `app/` with
PostgreSQL tests, Typst, private git dependencies):

```toml
version = 1

[resources]                    # per cargo session
cpu    = 5                     # with cpu_overcommit 2: three sessions per 8-core machine
memory = "4G"                  # reservation, not a limit

[environment]
image  = "rust:1-slim-trixie"
linker = "mold"

[environment.toolchains]
rust = "app/rust-toolchain.toml"   # pins the toolchain, also for local cargo

[environment.packages]
apt = ["cmake", "clang", "pkg-config", "libssl-dev", "python3", "curl",
       "ca-certificates", "git"]

[[environment.setup]]          # cargo nextest in the image (needed for the nextest split)
run = "curl -fsSL https://get.nexte.st/latest/linux | tar -xzf - -C /usr/local/bin && cargo nextest --version"

[[environment.setup]]          # private git dependencies: GH_TOKEN (see [secrets]) at fetch time
run = """
printf '#!/bin/sh\\n[ "$1" = get ] && printf "username=x-access-token\\\\npassword=%%s\\\\n" "$GH_TOKEN"\\n' > /usr/local/bin/git-credential-ghtoken \
 && chmod 755 /usr/local/bin/git-credential-ghtoken \
 && git config --system credential.https://github.com.helper /usr/local/bin/git-credential-ghtoken
"""

[environment.env]
CARGO_NET_GIT_FETCH_WITH_CLI = "true"
APP_TEST_DATABASE_URL = "postgres://postgres:app@127.0.0.1:5432/postgres"   # remote test runs

[services.postgres]            # throwaway server for tests that run remotely
image   = "postgres:17"
env     = { POSTGRES_PASSWORD = "app" }
port    = 5432
command = ["postgres", "-c", "max_connections=1000", "-c", "fsync=off"]

[secrets]                      # resolved locally per run, never stored remotely
GH_TOKEN = "cmd:gh auth token"

[cache.target]
scope    = "slot"
paths    = ["app/target"]     # stays on the machine, never synchronised
prune    = "rust"
max_size = "60G"

[cache.deps]
scope = "machine"
paths = ["$CARGO_HOME/registry", "$CARGO_HOME/git"]

[cache.sccache]
scope = "global"
tool  = "sccache"

[tool.cargo]
local = ["fmt", "publish", "login", "install", "new", "init", "add", "remove", "update"]
env   = ["RUSTFLAGS", "CARGO_TERM_*", "RUST_LOG", "RUST_BACKTRACE",
         "APP_TEST_SKIP_DB", "APP_TEST_KEEP_DB", "NEXTEST_*", "INSTA_*"]

[outputs]
mode = "fuse"                  # remote target/ at its local path

[editor]
rust_analyzer = "lsp-remote"

[prewarm]
run = ["cargo", "check", "--workspace", "--tests"]
```

## Toolchain

`[environment.toolchains] rust` installs the toolchain of the file into the
environment image (channel, components, targets). Use the same file
locally: local test runs, the output overlay and rust-analyzer's proc-macro
server all need the identical `rustc` version.

## Caches

* **Slot cache `target`**: the build directory stays on the machine in the
  repository's build slot, at most 24 hours (`ttl`, capped by the pool's
  `max_lifetime`), and is never copied to the laptop. Parallel sessions of
  the same repository get separate slots. `prune = "rust"` keeps it within
  `max_size`: old incremental sessions first, then compilation units not
  read for 5 runs, finally the whole slot
  ([caches-and-storage.md](../caches-and-storage.md#pruning)).
* **Machine cache `deps`**: the cargo registry and git checkouts, shared by
  all sessions of a machine (7 days by default).
* **Global cache `sccache`**: compiler results shared across machines
  through the pool's object store (aws-ec2 and local-docker have one, OCI
  and static need `[storage]`), so a fresh machine starts warm. sccache
  cannot cache incremental compilations of workspace crates; set
  `CARGO_INCREMENTAL = "0"` in `[environment.env]` (and locally, with the
  output overlay) to make them cacheable, at the price of slower
  incremental rebuilds on a warm slot.
* **Snapshots**: with a store, a slot can move to another machine and is
  restored lazily, fresh for cargo; the scheduler decides when that beats a
  cold build.

## Linker

`linker = "mold"` (or `"lld"`) installs the linker and selects it through
a `cc -fuse-ld=mold` driver, without touching `RUSTFLAGS`. Linking large
test binaries is often the slowest step of an incremental build; mold
shortens it considerably.

## Tests

`cargo test` and `cargo nextest run` are split: the test binaries are built
remotely (`cargo test --no-run`, or `cargo nextest archive`), transferred,
and run locally with cargo's environment, so tests use your local services
and environment variables. `cargo test --doc` runs remotely. Details in
[tools.md](../tools.md#tests-remote-build-local-run).

```sh
cargo nextest run -p app-store            # remote build, local run
OFFSHORE_SPLIT=0 cargo nextest run         # everything remote (uses [services])
```

Requirements for the local run: a local `cargo-nextest` for nextest and
`cargo-nextest` in the image; the test binaries must be executable here
(same architecture, a glibc at least as new as the image's; check with
`offshore why`, which says why tests ran remotely otherwise). Choose the
image's distribution no newer than your laptop's (e.g. `rust:1-slim-trixie`
on a trixie or newer host).

**Database tests.** Locally run tests use your local database (e.g. a
developer Postgres and your own `APP_TEST_DATABASE_URL`). `[services]`
and `[environment.env]` only apply to tests that run remotely
(`OFFSHORE_SPLIT=0`, doc tests, or when the binaries cannot run locally):
the service container shares the machine's network namespace and is
reachable on `127.0.0.1:5432`.

## Target overlay (`[outputs] mode = "fuse"`)

With the overlay the remote `app/target` appears at its local path:
local `cargo run`, `cargo test` and the test split use the remote artifacts
without copying. Local cargo only finds them fresh when what it
fingerprints matches ([tools.md](../tools.md#cargo-freshness)):

* the same `rust-toolchain.toml` locally (same `rustc -vV`);
* environment variables read by `env!` or build scripts the same on both
  sides (forward them via `[tool.cargo] env`, or set them in both places);
* `CARGO_INCREMENTAL` set identically;
* no local `$CARGO_HOME/config.toml` settings cargo fingerprints
  (`target.<triple>.linker`, `[profile]` overrides); local `rustflags` are
  passed to remote builds and must work there.

`offshore outputs status` shows the mount; `offshore outputs unmount`
discards local writes and shows the local `target/` again. The overlay
needs `/dev/fuse` and `fusermount3` locally (`offshore doctor` checks).

## rust-analyzer

```sh
offshore editor --mode lsp-remote vscode   # or zed, neovim; prints the settings
```

Flycheck and build scripts run remotely; proc macros and build script
outputs come from the output overlay, which the command mounts. With
`--mode local` they come back as eager outputs instead. rust-analyzer
itself, and `cargo metadata`, stay local.

## Private git dependencies

`[secrets] GH_TOKEN = "cmd:gh auth token"` resolves the token on the
laptop per run and passes it to the session only. The setup step installs a
git credential helper that reads `$GH_TOKEN`, and
`CARGO_NET_GIT_FETCH_WITH_CLI = "true"` makes cargo use git. `offshore init`
generates all three when a `Cargo.toml` has git dependencies on github.com.

## Verification

```sh
offshore doctor          # shims, configuration, secrets (resolved, not printed), image, FUSE
cargo build && offshore why       # remote, warm slot?, sccache hits, timings
cargo nextest run && offshore why # adapter cargo, phases: build remote, run local
offshore top --once      # machines and sessions
```

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `invalid .offshore.toml ... must be a plain directory inside the repository` | a `$CARGO_HOME/...` path in a cache without `scope = "machine"` (the default scope is `slot`) |
| tests run remotely with a note | local binaries not executable (glibc, architecture), no local or remote `cargo-nextest`; see the note and `offshore why` |
| local cargo rebuilds everything on the overlay | toolchain or environment differs (see Target overlay); `CARGO_LOG=cargo::core::compiler::fingerprint=info cargo build` names the cause |
| git dependency fetch fails | `gh auth token` fails locally, or the credential helper step is missing |
| every build is cold | different worktrees are different repositories (separate slots); `[prewarm]` and `offshore hooks install` warm new worktrees; check sccache hits in `offshore why` |
| out of memory kills | raise `[resources] memory` or the size; the history estimator learns peaks after a few runs |
