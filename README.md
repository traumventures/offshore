<p align="center"><img src="docs/assets/cover.jpg" alt="offshore: an oil rig at sea doing the heavy lifting, connected to the mainland" width="100%"></p>

# offshore

Run build and test commands on cloud machines as if they ran locally.

```console
$ cargo build          # ~/.offshore/bin/cargo -> offshore
   Compiling hello v0.1.0 (/home/me/hello)
    Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.61s
$ ./target/debug/hello # the binary was copied back
```

`offshore` is a single static Go binary. Invoked as `cargo`, `go`,
`bun`, `npm`, ... through a shim (or as `offshore run <tool> ...`), it
decides whether the command runs locally or on a build machine and then
behaves exactly like the local process: output, terminal, stdin, signals and
exit code. Sources are projected onto the machine at the same absolute path,
build directories stay warm on the machine for up to 24 hours, outputs come
back on demand, and when no machine is available the command runs locally.

Think of an offshore oil rig: the heavy lifting happens out at sea, on build
machines, and the rig is connected to the mainland (your laptop) by FUSE
pipelines that carry building materials (sources) out and bring the product
(build outputs) back. On the mainland everything looks as if the work was
done locally.

## Architecture

```mermaid
flowchart LR
  subgraph laptop[Laptop]
    tool["cargo / go / bun ...<br/>(shim = proxy)"] --> daemon["daemon<br/>watcher, manifests,<br/>image builds"]
  end
  daemon -- "leases (HTTPS + API key)<br/>or embedded" --> cp["orchestrator<br/>(embedded or shared<br/>control plane)"]
  cp -- "create / start / stop / destroy" --> prov["providers<br/>local-docker, aws-ec2,<br/>oci, static"]
  prov --> m
  subgraph m[Machine]
    sup["supervisor (VMs)"] --> worker["worker<br/>sessions in cgroups,<br/>build slots, caches"]
  end
  tool -- "QUIC: exec, sources (FUSE),<br/>write-back, outputs" --> worker
  worker -- "slot layers + pages, global caches" --> store[("object store")]
```

| Component | Runs on | Does |
|---|---|---|
| proxy | laptop | Invoked through a shim or `offshore run`. The tool adapter splits the command into phases (local or remote); the proxy runs the remote session and copies outputs back. |
| daemon | laptop | Watches sources, keeps versioned manifests, builds environment images, talks to machines, and runs the embedded orchestrator unless a control plane is configured. |
| orchestrator | laptop (embedded) or control plane | Places sessions, scales the pool, stops idle machines, destroys expired ones. |
| worker | machine | Projects the sources over FUSE at the same path, runs sessions in their own mount namespace and cgroup, keeps build slots and caches, writes changed sources back. |
| supervisor | VM and static machines | Starts the machine container layout, receives the machine secrets, enforces idle stop and the lifetime. |

## Quick start (local Docker, no cloud account)

Requirements: Linux, Docker, `/dev/fuse`, Go 1.26.

```sh
make build                                     # static binary in bin/offshore
bin/offshore shim install cargo
export PATH="$HOME/.offshore/bin:$PATH"    # in ~/.profile or ~/.zshrc
bin/offshore shim status                   # checks that the shims are first in PATH

cd ~/src/my-crate
offshore init --write                      # writes .offshore.toml
offshore doctor                            # checks installation and configuration
cargo build                                    # runs on a local-docker machine
offshore why                               # where and why it ran: timings (manifest barrier, build slot wait, run), units rebuilt
```

Without any client configuration the daemon embeds the orchestrator and
uses one local-docker provider (machines are Docker containers on this
host; set `OFFSHORE_DOCKER="sudo -n docker"` if your user cannot talk to
Docker). For cloud machines follow the [AWS](docs/guides/aws.md) or
[OCI](docs/guides/oci.md) guide; for a team, run a
[shared control plane](docs/control-plane.md).

## Documentation

Reference:

* [Configuration](docs/configuration.md): client, pool and repository files, every key
* [CLI](docs/cli.md): all commands, flags and environment variables
* [Tools, tests and outputs](docs/tools.md): tool adapters, test split, output overlay, port forwarding, rust-analyzer
* [Caches and storage](docs/caches-and-storage.md): cache scopes, media (local disk, instance store, volumes, object store) and their capability profiles, pruning, build slots on anyfs (commit, writeback, lazy restore), compiler cache
* [Scheduling](docs/scheduling.md): placement, scaling, overcommit, simulator
* [Control plane](docs/control-plane.md): shared orchestrator, TLS, container image, AWS deployment
* [Operations](docs/operations.md): metrics, logs, tracing, events, report, top, doctor, retention
* [Security](docs/security.md): identities, tokens, secrets, isolation
* [Compatibility](docs/compatibility.md): what works, what is limited, what stays local
* [Development](docs/development.md): tests, end-to-end tests, benchmarks

Guides:

* Providers: [AWS EC2](docs/guides/aws.md), [Oracle Cloud](docs/guides/oci.md)
* [Rust compile speed](docs/rust.md): measured compiler options, the parallel front end, wild, the opt-in headstart toolchain
* Ecosystems: [Rust](docs/guides/rust.md), [TypeScript / Node](docs/guides/typescript.md), [Go](docs/guides/go.md)

Project: [roadmap](docs/ROADMAP.md).
