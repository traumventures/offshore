# Rust compile speed on workers

What offshore provides for faster Rust builds, and what each project
turns on itself in `.offshore.toml`. Nothing here is on by default.

## Measurements (2026-11-11)

Workspace `app` (~120 crates, cargo-hakari, `debug =
"line-tables-only"`, dependencies at `opt-level = 1`) on a dedicated AWS
c7a.2xlarge (8 vCPU, the `x8` size), cargo with all 8 jobs, no sccache
unless noted. Cold: empty target directory, registry fetched. Warm:
incremental rebuild after changing one `const` string in `app-api`
(mean of two edits). CPU is the average utilisation of the run.

| variant | check¹ cold / warm | clippy² cold / warm | build³ cold / warm | CPU (cold) | notes |
|---|---|---|---|---|---|
| 1.98.1 + mold (today) | 188 / 22.1 s | 204 / 28.1 s | 218 / 22.9 s | 450–500 % | baseline |
| 1.98.1, default rust-lld | | | 216 / 22.9 s | 450 % | linker irrelevant for `--lib` |
| 1.98.1 + wild | | | 219 / 22.9 s | 445 % | idem |
| `debug = 0` | | | 210 / 22.7 s | 450 % | −4 % cold |
| deps `codegen-units = 16` | | | 220 / 22.6 s | 447 % | no effect |
| sccache, empty cache | | | 247 / 24.4 s | 163 % | +13 % (misses) |
| sccache, hot cache | | | 173 s | 226 % | −21 %; workspace crates (incremental) are never cached |
| nightly 2026-11-10 | 180 / 19.0 s | 191 / 23.4 s | 207 / 20.5 s | 440–500 % | newer compiler alone: −5..−10 % |
| nightly `-Zthreads=8` | 149 / 17.5 s | 166 / 20.4 s | 174 / 17.7 s | 620–700 % | −15..−21 % vs 1.98.1 |
| **1.98.1 `RUSTC_BOOTSTRAP=1 -Zthreads=8`** | **148 / 19.0 s** | | **165 / 19.4 s** | 640 % | −21 % cold, −14 % warm, pinned toolchain |
| nightly `-Zshare-generics=y` | | | 208 / 20.7 s | 437 % | = nightly |
| nightly Cranelift (workspace crates) | | | 203 / 20.4 s | 422 % | = nightly; the warm rebuild is front-end bound |

¹ `cargo check -p app-api -p app-server --all-targets`,
² `cargo clippy --workspace --all-targets`,
³ `cargo build -p app-api --lib`.

Linking a binary (`cargo build -p app-server --bin app-server`, linker
switched through the `cc` shim so nothing else rebuilds): relink after
touching `main.rs` mold 2.4 s, wild 1.9 s, rust-lld 3.3 s; after the
`app-api` edit mold 28.9 s, wild 28.5 s, rust-lld 30.0 s (means of 2-3
runs, first mold run excluded as page-cache warm-up).

Reading: the incremental rebuild of the large `app-api` crate runs on
one core (~100 % CPU) in the front end, so codegen-side options
(Cranelift, share-generics, codegen units, debuginfo, the linker for
`--lib`) change nothing; only the parallel front end attacks it.

## Parallel front end (`-Zthreads`)

The parallel front end is built into stable rustc but gated behind `-Z`.
`RUSTC_BOOTSTRAP=1` allows it on the pinned toolchain, so
`rust-toolchain.toml` stays as it is:

```toml
[environment.env]
RUSTC_BOOTSTRAP = "1"
CARGO_BUILD_RUSTFLAGS = "-Zthreads=8"
```

Caveats: `RUSTFLAGS` from the client (passed through `[tool.cargo] env`)
replaces `CARGO_BUILD_RUSTFLAGS`, so add `-Zthreads` there when you set
it. `RUSTC_BOOTSTRAP=1` lets build scripts that probe for nightly
features find them (app builds, checks and lints cleanly with it). The
flag is part of cargo's fingerprint: a local cargo without it (rust-analyzer,
`[outputs] mode = "fuse"` freshness) rebuilds instead of reusing the
remote target directory. It uses more memory (peak RSS +5 %) and CPU per
session; set `[resources] cpu` accordingly.

## Linker

`[environment] linker = "mold" | "lld" | "wild"`, see
[caches-and-storage.md](caches-and-storage.md#linker). `wild` installs
the pinned static release of [wild](https://github.com/wild-linker/wild)
and relinks the 1 GB-class `app-server` debug binary about 0.5 s faster
than mold (1.9 vs 2.4 s); it is younger than mold, so keep mold where a
link fails. The default rust-lld is the slowest of the three (3.3 s).

## headstart toolchain

[headstart](https://github.com/PowderworksCode/headstart) is a patched
rustc (`-Zearly-metadata`) and cargo (`-Zheadstart`) that start dependent
crates on an early metadata file, before their dependencies have checked
function bodies. Upstream ships patches only; offshore builds the
toolchain and provides it in the environment image:

```toml
[environment.toolchains]
rust  = "rust-toolchain.toml"
extra = ["headstart"]        # rustup linked toolchain "headstart"
```

The image then contains `/opt/offshore/toolchains/headstart` linked as
rustup toolchain `headstart` (rustc and cargo only: no clippy, rustfmt,
rustdoc, no extra targets). The project chooses when to use it, e.g. for
a check-only command or session environment:

```toml
[environment.env]            # or per command: cargo +headstart check -Zheadstart
RUSTUP_TOOLCHAIN = "headstart"
CARGO_UNSTABLE_HEADSTART = "true"
CARGO_INCREMENTAL = "0"
RUSTC_WRAPPER = ""            # sccache would turn headstart off
```

* **Opt-in, experimental.** It is a dev build of rustc 1.101 (rustc
  `6006fd0`, cargo `4f3fb24`, headstart `02ec0bf314`), not the pinned
  toolchain: results can differ from your CI compiler, and a target
  directory is not shared between the two.
* **Incremental ICE.** Upstream has an open ICE where the early and full
  metadata hashes (SVH) disagree in incremental builds
  (`docs/ice-early-metadata-svh.md` upstream). Use it with
  `CARGO_INCREMENTAL=0` or for clean `cargo check` runs only.
* **No compile cache.** sccache, or any `rustc-wrapper` that buffers
  rustc's output, hides the early metadata and turns headstart off. Unset
  `RUSTC_WRAPPER` for headstart runs (an `[cache.*] tool = "sccache"`
  sets it).
* **`-Zheadstart`** (or `CARGO_UNSTABLE_HEADSTART=true`, or `[unstable]
  headstart = true`) is a cargo flag; without it the patched cargo behaves
  like upstream.
* x86_64 Linux only; the archive is built on Debian bookworm (glibc 2.36)
  and runs on bookworm and newer images.

The archive is `s3://offshore-artifactbucket-z7c0j8lsimso/toolchains/headstart-02ec0bf314-x86_64-unknown-linux-gnu.tar.gz`
(238 MB, 832 MB unpacked, SHA-256 pinned in `internal/envbuild/tools.go`).
The bucket is private: the client that builds the environment image
downloads it with its AWS credentials (default chain, e.g. `AWS_PROFILE`)
or takes it from a pre-seeded `OFFSHORE_TOOL_CACHE`
(`<sha256>-<file name>`). Rebuild it with `deploy/headstart/build.sh` (about
20 minutes on 16 vCPU), upload, and update `headstartVersion` and the
checksum.
