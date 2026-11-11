# Compatibility

Machines are Linux, x86_64 or arm64 (`resources.arch`; arm64 sizes on
aws-ec2 and oci). Clients are Linux; test binaries and outputs that run
locally must match the client's OS, architecture and C library (the proxy
checks and otherwise runs tests remotely). Everything that builds or tests
through a proxied tool on Linux works; what needs local hardware, a GUI or
another OS stays local.

## Works remotely

Output identical to a local run.

* Rust: `cargo build`, `check`, `test`, `clippy`, `doc`, `bench`, `run`
  (also with forwarded ports), `fmt`; `cargo nextest`; `cargo hack`,
  `cargo llvm-cov`, `cargo miri`, `cargo fuzz`, `cargo udeps`,
  `cargo expand`, `cargo deny`, `cargo audit`, `cargo insta`
* cross compilation with target toolchains: `cargo zigbuild`, `cargo xwin`,
  `wasm-pack build`, `trunk build`, `maturin build`
* linkers `mold`, `lld` and `wild` (`[environment] linker`)
* the headstart toolchain (`[environment.toolchains] extra = ["headstart"]`, see [rust.md](rust.md))
* Go: `go build`, `vet`, `run`, `mod`, `generate`, `go test` (split or
  remote), cgo with the image's C toolchain
* Node: `bun`, `npm`, `pnpm`, `yarn` installs (into the machine's
  `node_modules` cache), scripts and tests; `npx`/`bunx`
* wrappers such as `bacon`, `cargo watch`, `just` and `make` run locally;
  the `cargo`/`go`/`bun` they start goes through the proxy
* tests split into a remote build and a local run: `cargo test`,
  `cargo nextest run`, `go test` ([tools.md](tools.md#tests-remote-build-local-run))

## Limited

* rust-analyzer stays local; flycheck and build scripts run remotely
  (`offshore editor`). Proc-macro dylibs come back as eager outputs or
  through the output overlay (`lsp-remote`); a full LSP bridge does not
  exist.
* Local cargo on remote artifacts (`[outputs] mode = "fuse"`) needs the
  same toolchain and cargo configuration locally ([tools.md](tools.md#cargo-freshness)).
* `cargo tarpaulin` needs `ptrace`, which sessions do not get; use
  `cargo llvm-cov`.
* `maturin develop` installs into the local virtualenv: build remotely with
  `maturin build`, install the wheel locally.
* `cross` and anything else that needs Docker in the session (not
  available): use `zigbuild` or target toolchains in the environment image.
* Node packages with native modules are installed for the machine's
  platform; a remote `node_modules` is never copied to a laptop of another
  platform, so local scripts need their own local install.
* Bazel and Buck2 have their own remote execution; use that instead.

## Local only

* flashing and debugging hardware (`probe-rs`, `espflash`, `cargo embed`):
  build remotely, flash locally (`offshore fetch` or outputs).
* dev servers (`trunk serve`, `tauri dev`, `vite`) can run remotely with
  port forwarding (`[tool.X] ports`); anything that opens local windows or
  devices runs locally.
* macOS, iOS and Windows-native targets (other than `xwin` builds) need the
  respective host.

## Host file systems

The cache medium of cloud machines is xfs with reflink (formatted by the
supervisor; anything else runs degraded and is reported, see
[operations.md](operations.md#status-why-and-doctor)). local-docker and
static hosts run on any Linux file system: xfs or btrfs (reflink) behave
like cloud machines; ext4, overlay or tmpfs work, with session snapshots
copying instead of cloning and warnings from the worker and
`offshore doctor`. ext4's `noauto_da_alloc` mount option is warned about
but safe for offshore's own files, whose commit points are fsynced.

Only tools with an installed shim are proxied; `OFFSHORE_DISABLE=1`
always runs locally.
