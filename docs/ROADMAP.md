# Roadmap

Status as of November 2026. The detailed implementation table (German) is
section 0 of [PLAN.md](PLAN.md).

## Done

* Transparent proxy (shims, `cloudsourced run`), PTY, signals, stdin, exit
  codes, local fallback, one retry on machine loss.
* Source projection over FUSE at the same path, barrier per run, write-back
  with conflict detection, `watch`/`immutable`/`hidden` rules.
* Content addressed environment images (image or Dockerfile, apt,
  toolchains, setup steps, services), background prebuild, per architecture.
* Worker sessions in mount namespaces and cgroups, exclusive build slots
  (at most 24 h), services, port forwarding, secrets per run.
* Caches v2: `[cache.<name>]` with slot, machine (also per lockfile digest
  with remote install) and global scope (sccache, ccache, Go); pruning; the
  object store (`[storage]`, scoped worker grants); snapshots with lazy
  restore; disk growth on aws-ec2.
* Outputs: eager globs, `cloudsourced fetch`, writable output overlay with
  cargo freshness; rust-analyzer `local` and `lsp-remote`.
* Tool adapters (cargo, go, node, generic); tests built remotely and run
  locally (`cargo test`, `cargo nextest run`, `go test`).
* Providers: local-docker, aws-ec2 (instance type fallback, arm64), oci
  (automatic compartment/VCN/bucket/OCIR/IAM, flex shapes), static hosts.
* Scheduler (bin packing, consolidation, warm pool windows, overcommit,
  history estimator), simulator and `sim export`.
* Shared control plane: HTTPS with API key, fingerprint pinning or
  `--hostname` with Let's Encrypt, optional mutual TLS, container image,
  `deploy/aws/controlplane.yaml`; machine identities derived from one
  master key, tokens per lease; IAM template `deploy/aws/iam.yaml`.
* Operations: Prometheus metrics, JSON logs, OpenTelemetry tracing, event
  log, `report`, `top`, `why`, `status`, `doctor`, `init`, labels for
  agents and worktrees, prewarm and git hook.

## In progress

* Scheduling v2: one cost model for every decision with
  `[scheduler] cost_optimization` (0 = fastest result, 1 = cheapest),
  disk-aware placement and slot moves (prune, resize, snapshot and
  restore). See [scheduling.md](scheduling.md).

## Later

* **Users and quotas**: per-user API keys, quotas and cost limits on the
  control plane (today one global API key per pool).
* **Production domain**: run the shared control plane on a DNS name via
  `--hostname` (supported, see [control-plane.md](control-plane.md#from-no-domain-to-a-domain);
  current deployments pin a self-signed certificate).
* **GPU machines**: sizes and environments with GPUs (drivers in the
  machine image, device access in sessions).
* **Live arm64 on OCI**: Ampere A1 sizes are implemented, but only amd64
  shapes were tested live; run the OCI smoke test on A1.
* More providers (Scaleway, Kubernetes, ECS) and nextest across several
  machines remain open by choice.
