# Development

Go module `github.com/traumventures/offshore` (Go 1.26), one static
binary (`CGO_ENABLED=0`). Layout: `cmd/offshore` (CLI), `internal/*`
(proxy, daemon, orchestrator, scheduler, worker, supervisor, providers,
adapters, caches, snapshots, ...), `deploy/` (control plane image and AWS
templates), `test/` (end-to-end tests and benchmarks). The design plan
(German) is [PLAN.md](PLAN.md), open work is in [ROADMAP.md](ROADMAP.md).

## Build and unit tests

```sh
make build      # bin/offshore plus bin/offshore-linux-<arch> for the other machine architecture
make test       # go test ./...: unit and property tests
make vet
gofmt -l .      # must print nothing
make image DOCKER="sudo -n docker"   # control plane container image
```

Policies, the simulator, adapters, configuration and providers (with fakes)
are covered by unit tests; `internal/scheduler/sim` checks the policy
claims of [scheduling.md](scheduling.md) on sample traces
(`TestSampleTraces`).

## End-to-end tests

```sh
E2E_DOCKER="sudo -n docker" make e2e                            # full run (local-docker)
E2E_PARALLEL=1 E2E_DOCKER="sudo -n docker" ./test/e2e/run.sh    # parts concurrently
E2E_REMOTE=1 E2E_DOCKER="sudo -n docker" ./test/e2e/run.sh      # against a control plane
```

Requirements: Docker, `/dev/fuse`, a static build at `bin/offshore`.
The test is split into parts (`test/e2e/parts`: `core` without compiling,
`rust`, `lifecycle`), selectable with `E2E_PARTS`; each part has its own
owner, daemon, ports and test clock. Idle stop and lifetime expiry use the
test clock (`OFFSHORE_TEST_CLOCK=offset:FILE`, moved with the hidden
`offshore debug clock advance DURATION`) and a fast reconcile loop
(`OFFSHORE_RECONCILE_INTERVAL`), so they take seconds. In remote mode
only `core` runs unless `E2E_FULL=1`. The environment image is reused across
runs (`E2E_CLEAN_IMAGES=1` removes the images a run built; `E2E_KEEP=1`
keeps the work directories with state and logs).

The end-to-end test builds a Rust project remotely and verifies outputs,
write back, exit codes, signals, stdin, environment allowlist, secrets,
services, projection consistency under rapid changes, shims, lazy outputs,
dependency caching, compiler cache hits on a fresh machine, the mold
linker, parallel sessions, scale out, idle stop, warm restart, lifetime
expiry, fail-open and cleanup.

`deploy/docker/smoke-test.sh` starts the control plane image like
[control-plane.md](control-plane.md#local-docker-host-local-docker-provider)
describes and runs a real build through it.

## Cloud smoke tests

Cloud tests are opt-in and create real resources; clean up afterwards.

* OCI: `internal/orchestrator/ocismoke_test.go` (build tag `ocismoke`):
  `go test -tags ocismoke -run 'TestOCILimits|TestOCISmoke' -v
  ./internal/orchestrator`, cleanup with `-run TestOCICleanup`.
* AWS: configure an aws-ec2 pool ([guides/aws.md](guides/aws.md)) and build
  a small crate (e.g. one depending on serde) with
  `OFFSHORE_REQUIRE_REMOTE=1`; `offshore down` afterwards.

## Benchmarks

| script | measures |
|---|---|
| `test/bench/overlay-fresh.sh` | local cargo freshness on the output overlay ([tools.md](tools.md#cargo-freshness)) |

## Operator tools

`offshore debug token|probe` mints and tests machine tokens;
`offshore sim` replays traces against policies
([scheduling.md](scheduling.md#simulator)).
