# Tools, tests and outputs

## Shims and the proxy

`offshore shim install [tools...]` creates symlinks in
`~/.offshore/bin` (`$OFFSHORE_SHIM_DIR`); without arguments it
installs the tools of the current repository's `[tool.*]` sections. With
that directory first in `PATH`, plain `cargo build`, `go test` or `bun
install` in a repository with a `.offshore.toml` go through
offshore, for people, scripts and coding agents alike. `offshore
shim status` exits 1 when a shim is missing from `PATH` or shadowed by an
earlier entry. `offshore run <tool> ...` is the explicit form without
shims. Outside a repository with a configuration, and for tools without a
shim, the local tool runs unchanged.

A run goes through these steps:

* **Barrier.** Every run carries the manifest version of each root. The
  worker starts the process only after it applied that version, so the
  remote side never sees stale files. Before each run the daemon does a stat
  walk (hashing only changed files); the watcher pushes changes earlier.
* **Projection.** `lookup`/`getattr`/`readdir` on the machine are answered
  from the manifest without a network round trip; kernel caches are kept for
  an hour and invalidated precisely when deltas arrive. Content is fetched
  once per hash into a content addressed store.
* **Writes** to sources (e.g. `Cargo.lock`, `cargo fmt`) go to an overlay
  and are written back after the run, unless the local file changed
  meanwhile.
* **Outputs**: `[tool.X] eager` globs are copied back after every run;
  `offshore fetch <glob>...` copies others from the latest remote build
  (zstd compressed); the [output overlay](#output-overlay) shows the whole
  build directory.

Escape hatches: `OFFSHORE_DISABLE=1` (always local),
`OFFSHORE_REQUIRE_REMOTE=1` (fail instead of falling back),
`OFFSHORE_STDIN=always|never`, `OFFSHORE_VERBOSE=1`,
`OFFSHORE_SPLIT=0`, `OFFSHORE_NO_RETRY=1`. When no machine can be
provisioned (or a secret cannot be resolved) before anything ran remotely,
the proxy falls back to the local tool. If the machine is lost during a run,
the proxy retries once on another machine ([operations.md](operations.md#machine-loss)).

## Tool adapters

Every proxied command goes through the adapter of its tool
(`internal/adapter`): it classifies the command line into phases (`deps`,
`build`, `run`), each `local` or `remote`, names the artifacts a phase
produces with their cache scope, and what a local phase needs.
`[tool.X] adapter` picks the adapter; by default it follows the tool name
(`cargo`; `go`; `bun`, `bunx`, `npm`, `npx`, `pnpm`, `yarn` are `node`;
anything else `generic`). `offshore why` shows the adapter and the
phases of a run.

| adapter | remote as typed | split: remote build, local run | default caches (`init`) |
|---|---|---|---|
| cargo | build, check, clippy, run, ... | `cargo test`, `cargo nextest run` | `target` slot (prune rust), `deps` machine, `sccache` global |
| go | build, vet, run, mod, ... | `go test` | `go-build` global (GOCACHEPROG), `go-mod` machine, `go-out` slot |
| node | install/ci/add (deps), scripts, tests | none (`local = ["test", "run dev"]` runs them here) | `node-modules` machine, keyed by `package.json` + lockfile, with the lockfile's install command |
| generic | everything not in `local` | none | none |

Local platform checks (OS, architecture, C library family and version,
dynamic loader, shared libraries) use `internal/platform`, which also feeds
the environment image hash and the key of machine caches (OS/arch/libc
without version). `OFFSHORE_PLATFORM=linux/amd64/glibc-2.39` overrides
the detection.

## Tests: remote build, local run

`cargo test`, `cargo nextest run` and `go test` are split: the test
binaries are built remotely and run on this machine, with the local
environment and local services (e.g. the developer's Postgres). Type the
command as usual (`cargo nextest run -p store foo`, `cargo test foo --
--nocapture`, `go test -run X ./...`); output and exit code are those of a
local run.

* **cargo test**: the remote phase runs `cargo test --no-run
  --message-format=json-render-diagnostics` with the build options; the
  executables it reports are transferred and run in cargo's order with
  cargo's working directory (package root), environment
  (`CARGO_MANIFEST_DIR`, `CARGO_PKG_*`, `CARGO_BIN_EXE_*`, cargo `[env]`
  configuration including `force` and `relative`, the build script's
  `rustc-env` values and `OUT_DIR`, whose files are transferred too),
  arguments (test name, `--quiet` with `-q`, everything after `--`), status
  lines and `error: test failed, to rerun pass ...` messages,
  `--no-fail-fast` summary and exit code 101. Doc tests run remotely
  afterwards (`cargo test --doc` with the same options), as cargo runs them
  last.
* **nextest**: the remote phase runs `cargo nextest archive` with the cargo
  build options (`-p`, `--workspace`, features, `--cargo-profile`, ...);
  the zstd archive is transferred and `cargo nextest run --archive-file
  ...` runs locally with the filters, runner and reporter options,
  `-P`/`--profile`, `-- args` and `NEXTEST_*` variables. Configuration
  options (`--config-file`, `-P`) go to both.
* **go test**: the remote phase lists the packages and runs `go test -c`
  per package with the build flags (`-race`, `-tags`, ...; results come
  from the global go cache); the binaries run locally in their package
  directories with the test flags as `-test.*` (`-run`, `-v`, `-count`,
  `-timeout`, ..., `-args`), printing go test's `ok`/`FAIL`/`?` lines and
  exiting 1 on failure. The binaries travel in the first slot-scope cache
  directory (`init` proposes `[cache.go-out] paths = [".offshore-build"]`).

Commands that do not run tests (`--no-run`, `--doc`, `--message-format`,
`nextest list`, `nextest archive`, reuse options such as `--archive-file`;
`go test -json`, `-c`, `-o`, `-cover*`, profiles, `-fuzz`, `-list`) and
unknown options run remotely as typed. The tests run remotely instead,
with one line saying why, when this machine cannot execute the binaries:
another OS or architecture, another C library or an older glibc than the
environment image, a missing shared library or dynamic loader (checked on
the ELF files), no local `cargo-nextest`, no `cargo-nextest` in the
environment image, nextest failing to list the tests locally (exit 104), or
test binaries outside the slot-scope cache paths.

`[tool.X] split` lists what is split (cargo: `["test", "nextest"]`, go:
`["test"]`, both the default); `split = []` turns it off for a tool,
`OFFSHORE_SPLIT=0` for one run. Transport: with `[outputs] mode =
"fuse"` the binaries appear through the output overlay
(`daemon.EnsureOutputs` after the build's release, nothing is copied);
otherwise they are fetched over the session that built them, zstd
compressed, only files whose content changed.

Not reproduced locally: files a test reads at run time from outside
`OUT_DIR` and the build directories (e.g. generated next to the sources by
a remote build script), cargo `[env]` set by `--config` on the command line,
and nextest's own handling of build-script values (whatever `cargo nextest
run --archive-file` does locally).

## Output overlay

By default only `eager` outputs come back after a run; `offshore fetch`
copies others on request. With the output overlay the remote build
directories (the `paths` of slot-scope caches, e.g. `target/`) appear at
their local path as if they had been built locally: local `cargo run`,
`cargo test`, `cargo nextest` use the remote artifacts without rebuilding
and without `offshore fetch`.

```toml
[outputs]
mode = "fuse"      # mounted by the daemon for this repository
```

```sh
offshore outputs mount      # mount by hand (also without mode = "fuse")
offshore outputs status     # mount points, upper layers, generation
offshore outputs unmount    # unmount and discard local writes
```

- The mount (owned by the daemon, `fusermount3`, file system type
  `fuse.offshore-outputs`) is a writable union implemented with go-fuse:
  the anyfs userspace overlay (`anyfs.UserOverlay`, newest-mtime merge)
  over the remote slot as an anyfs Source (`anyfs.RemoteTree`, the peer
  tier; `internal/anyfs`). Lower layer: the build slot of the latest remote
  build of the repository. Upper layer: `$STATE/outputs/upper/<hash>`, a
  local directory next to the client state, never in the repository; every
  write goes there (cargo's locks and fingerprints, local rebuilds, copied
  up files). The local `target/` below the mount point is hidden while
  mounted and untouched; it is all there again after `unmount`.
- Kernel overlayfs is not used: an unprivileged user can only mount it in a
  user namespace, invisible to the shell that runs cargo.
- Directories are listed on first access from the machine of the latest
  build, file contents are downloaded whole on first open into a local
  content cache (`$STATE/outputs/cas`, keyed by BLAKE3) and reused as long as
  path, size and mtime are unchanged. Every remote run starts a new
  generation: listings and kernel caches are dropped, unchanged contents are
  kept. Exact mtimes (nanoseconds) are served, which cargo compares.
- Layer rules: for a path present in both layers the newer mtime wins, ties
  go to upper. Writing a remote file copies it up with its remote mtime
  (`O_TRUNC` without download); deleting or replacing a remote entry writes
  a whiteout marker `.wh.NAME` whose mtime hides remote entries up to that
  time; a directory created over a deleted remote one gets an opaque marker.
  So a newer remote build replaces older local state at the paths it
  produced, and local rebuilds win over older remote outputs. Names starting
  with `.wh.` cannot be created in the mount. Directories with remote
  content cannot be renamed (`EXDEV`); `mv` falls back to copying.
- Eager outputs inside a mounted directory are stored in the content cache
  instead of being written below the mount point.
- When the machine is stopped or gone, cached listings and contents are
  still served; uncached files fail with `EIO`, uncached directories look
  empty, and the daemon log says which machine was unreachable. The machine
  is not asked again for 20 s.
- The mount and its upper layer survive daemon restarts (state in
  `$STATE/outputs/state.json`).
- Go API: `daemon.EnsureOutputs(ctx, root)` mounts the overlay of a
  repository (starting the daemon) and returns its status.

`$STATE` is the daemon state directory (`$OFFSHORE_STATE_DIR`, else
`$XDG_STATE_HOME/offshore` or `~/.local/state/offshore`).

### Cargo freshness

Local cargo considers the remote build fresh when its fingerprints match.
With `mode = "fuse"` every remote cargo session (commands of a tool with
the cargo adapter, including `[prewarm] run`) aligns what cargo hashes; the
cargo adapter supplies the tool homes and rustflags (`Plan.AlignOutputs`),
other adapters need none:

- Sources are projected at the local path (identical absolute paths, mtimes).
- The image's `CARGO_HOME` and `RUSTUP_HOME` appear at the client's paths
  (`$CARGO_HOME`/`$RUSTUP_HOME` or `~/.cargo`/`~/.rustup`), bind mounted in
  the session's mount namespace; `PATH` and the variables point there.
  Cargo hashes the absolute paths of registry sources
  (`$CARGO_HOME/registry/src/...`), so `$CARGO_HOME/registry` machine caches
  also live at the client's path. The path of `rustc` is not hashed, its
  version (`rustc -vV`) is: pin it with `[environment.toolchains] rust` and
  use the same `rust-toolchain.toml` locally.
- The configured linker (`[environment] linker`) is selected through a `cc`
  on the session's `PATH` instead of `CARGO_TARGET_<triple>_LINKER`, which
  cargo hashes.
- `build.rustflags` and `target.<host>.rustflags` from the client's
  `$CARGO_HOME/config.toml` (not projected) are passed as
  `CARGO_BUILD_RUSTFLAGS`/`CARGO_TARGET_<HOST>_RUSTFLAGS` unless the run
  forwards `RUSTFLAGS` or those variables. They must work remotely (e.g. a
  `-fuse-ld=mold` needs `linker = "mold"`).

Measured with `test/bench/overlay-fresh.sh` (local-docker, a crate with a
proc macro, `itoa` and `libc` with a build script, a test; local cargo with
a global config setting `rustc-wrapper` and target rustflags): after a
remote `cargo build` and `cargo test --no-run`, local `cargo build` and
`cargo test --no-run` recompile 0 of 5 units, the test and binary run
locally, and after an edit and a remote rebuild local cargo again
recompiles 0 units; a local edit recompiles only the edited crate (into the
upper layer).

Known gaps (cargo rebuilds the affected units locally, which is correct but
slower): environment variables that differ locally and are tracked by
`env!`/`option_env!` or a build script's `rerun-if-env-changed` (e.g. `CC`
from a local `[env]` section); `CARGO_INCREMENTAL=0` set only remotely (set
it locally too); other settings of the local `$CARGO_HOME/config.toml` that
cargo fingerprints (`target.<triple>.linker`, `[profile]` overrides); a
different local toolchain version.

## Port forwarding

While a remote session runs, the proxy listens on `127.0.0.1:<local>` for
every port of `[tool.X] ports` (or `OFFSHORE_PORTS=8080,3000:3001`,
which replaces the configured list) and carries each connection over its
own QUIC stream to `127.0.0.1:<remote>` in the machine's network namespace,
which sessions and service containers share. Connections end with the
session. A busy local port is reported and skipped; the command still runs.

## rust-analyzer

`offshore editor [--mode local|lsp-remote] vscode|zed|neovim` prints
settings that run flycheck and build scripts through the proxy.
rust-analyzer stays local; because paths are identical, diagnostics need no
rewriting. Proc-macro libraries and build script outputs must be available
locally:

- `--mode local` (default): via eager outputs (`target/debug/deps/*.so`,
  `target/debug/build/*/out/**`).
- `--mode lsp-remote` (or `[editor] rust_analyzer = "lsp-remote"`): via the
  output overlay, which the command mounts. The paths rust-analyzer gets
  from the remote `cargo check --message-format=json` (proc-macro `.so`,
  `OUT_DIR`) resolve inside the mount. `cargo metadata` stays local
  (dependency sources must exist locally). The local toolchain must match
  the remote one (`rust-toolchain.toml`), otherwise rust-analyzer's
  proc-macro server rejects the libraries.

## Environment image prebuild

When the environment of a repository changes (`.offshore.toml`, the
toolchain file, setup `inputs`, the Dockerfile directory: anything that
changes `offshore env hash`), the daemon builds the new image in the
background right away: on the file watcher event (debounced 2 s) and, when
no event was seen, at the next request. The build goes to every provider
and platform of the pool, including pushes to remote registries (ECR, OCIR)
that would otherwise wait for the first machine; with a shared control
plane the daemon uploads the context and the control plane builds detached
from the request. A run that needs the image meanwhile joins the running
build. `offshore env build [--wait]` starts it explicitly,
`offshore env status [--json]` shows where the image exists and the
last background build; failures are in the daemon log and `env-build.log`.
