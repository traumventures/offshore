# Guide: Go

The `go` adapter runs `go build`, `vet`, `run`, `mod`, `generate` and
friends remotely as typed and splits `go test`: test binaries are compiled
remotely and run locally. Go's build cache is shared across machines.

## Prerequisites

* A configured pool ([README](../../README.md#quick-start-local-docker-no-cloud-account), [AWS](aws.md), [OCI](oci.md)).
* `offshore shim install go`, shims first in `PATH`.
* A Go toolchain locally only for things that stay local (none by
  default); the local test run executes compiled binaries and needs no Go.
* Go 1.24 or newer in the image (`GOCACHEPROG`).

## Setup

1. `offshore init` detects `go.mod`/`go.work`, picks `golang:<go
   directive>-trixie` (e.g. `golang:1.26-trixie`), and proposes the three
   default caches and a `[tool.go]` section. Write it with
   `offshore init --write`.
2. Add `.offshore-build/` to `.gitignore` (the slot directory that carries
   test binaries).
3. `offshore doctor`, then `go build ./...` and `offshore why`.

## Example configuration

```toml
version = 1

[resources]
cpu    = 4
memory = "4G"

[environment]
image = "golang:1.26-trixie"

[environment.packages]
apt = ["libsqlite3-dev"]          # cgo dependencies, if any

[cache.go-build]                  # go build results shared across machines (GOCACHEPROG)
scope = "global"
tool  = "go"

[cache.go-mod]                    # downloaded modules (GOMODCACHE), per machine
scope = "machine"
paths = ["$GOPATH/pkg/mod"]

[cache.go-out]                    # test binaries of split go test runs
scope = "slot"
paths = [".offshore-build"]

[tool.go]
env = ["GOFLAGS", "GOPRIVATE", "GOEXPERIMENT", "CGO_ENABLED"]
eager = ["bin/*"]                 # e.g. binaries built with go build -o bin/
```

## Caches

* **`GOCACHE` global (`tool = "go"`)**: the worker sets `GOCACHEPROG` to
  `offshore gocacheprog`, which keeps outputs on the machine disk (go reads
  them from there) and shares them through the pool's object store, keyed by
  the environment hash. A fresh machine therefore compiles only what no
  earlier machine compiled within 24 hours. aws-ec2 and local-docker have a
  native store; OCI and static hosts need `[storage]`, otherwise the cache
  stays per machine.
* **`GOMODCACHE` per machine**: `$GOPATH/pkg/mod` (`/go/pkg/mod` in the
  official images) is shared by all sessions of a machine for 7 days by
  default; `go mod download` fills it.
* **`go-out` slot**: split `go test` runs put the compiled test binaries in
  the first slot cache directory, from where they are transferred to the
  laptop.

## Tests

`go test` is split: the remote phase lists the packages and runs `go test
-c` per package with the build flags (`-race`, `-tags`, ...; results come
from the global cache); the binaries run locally in their package
directories with the test flags as `-test.*` (`-run`, `-v`, `-count`,
`-timeout`, `-args`), printing `ok`/`FAIL`/`?` lines like `go test` and
exiting 1 on failure.

```sh
go test ./...                 # remote build, local run
go test -run TestStore -v ./internal/store/...
OFFSHORE_SPLIT=0 go test ./...   # everything remote
```

`go test -json`, `-c`, `-o`, `-cover*`, profiles, `-fuzz` and `-list` run
remotely as typed. Tests run remotely (with a note) when the binaries
cannot execute here: another OS or architecture, cgo binaries linked
against a newer glibc than the laptop's, a missing shared library. Pure Go
binaries (`CGO_ENABLED=0`) run anywhere with the same OS and architecture.
Files a test reads at run time come from the local checkout, which has the
same content as the remote projection.

## Private modules

Forward `GOPRIVATE` (and `GOFLAGS`) via `[tool.go] env`, and give the
session credentials with a secret plus a setup step, as for Rust's git
dependencies: a git credential helper that reads the secret
([rust.md](rust.md#private-git-dependencies)), or a `.netrc` written by a
setup step from the secret at run time.

## Verification

```sh
offshore doctor
go build ./... && offshore why   # adapter go, remote, cache counters
go test ./... && offshore why    # phases: build remote, run local
offshore top --once
```

## Troubleshooting

| symptom | cause and fix |
|---|---|
| tests run remotely with a note about the binaries | glibc or architecture mismatch; build with `CGO_ENABLED=0` or use an image with an older glibc |
| `test binaries outside the slot-scope cache paths` | add the `go-out` slot cache |
| every build on a new machine is slow | no object store for the global cache (OCI/static need `[storage]`) |
| `GOCACHEPROG` not honoured | the image's Go is older than 1.24 |
| module download fails for private modules | credentials and `GOPRIVATE` (see above) |
