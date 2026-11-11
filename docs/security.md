# Security

## Identities and transport

* **Control plane API.** HTTPS with one global API key (`<state>/api-key`,
  never logged; clients reference it with `env:`, `file:` or `cmd:`).
  Without a domain clients pin the persistent self-signed certificate by
  fingerprint; with `--hostname` the certificate comes from Let's Encrypt.
  `--client-ca` adds mutual TLS. `/healthz` is the only unauthenticated
  endpoint (plus `/metrics` on the optional `--metrics-listen` listener,
  which should stay on localhost). See [control-plane.md](control-plane.md).
* **Machines.** Worker connections use QUIC with TLS 1.3. Each machine's
  Ed25519 key and self-signed certificate are derived from the pool master
  key (HKDF-SHA256 over the machine id; the certificate is deterministic in
  id and expiry), so clients pin a fingerprint the orchestrator can always
  recompute and provider labels need not be trusted. A machine only
  receives the seed of its own key (`CLOUDSOURCED_TLS_SEED`, secret) and the
  control plane's public key (`CLOUDSOURCED_SIGNER_PUB`).
* **Tokens.** Every stream carries a token signed by the control plane
  (Ed25519): claims `scope`, `machine`, `lease`, `exp`, `iat`. Sessions,
  source sync and output fetch need a `session` token, which is minted per
  lease, valid until the machine expires and only for that lease's
  sessions; status needs a `probe` token (minted per probe, two minutes);
  `admin` allows everything (slot prune, save, restore). There is no shared
  per-machine secret. `cloudsourced debug token|probe` mints and tests
  tokens for operators.
* **Master key.** `<state>/master.key` of the control plane (or the embedded
  daemon) is the root of all machine identities and tokens. Protect it like
  a CA key; replicas import it with `CLOUDSOURCED_MASTER_KEY(_FILE)`.

## Machine secrets

User data or instance metadata carries only the public machine spec. The
secret environment (key seed, signer key, telemetry headers) travels on the
provider's secret channel and is read once by the supervisor:

| provider | secret channel |
|---|---|
| aws-ec2 | SSM SecureString `/cloudsourced/<stack>/machines/<id>`, read with the instance role and deleted; the reaper deletes orphans after 15 min |
| oci | object `secrets/<id>.json` in the bucket, read with the instance principal and deleted |
| static | written by `supervisor static apply` over SSH, root-only, read once |
| local-docker | container environment variables (visible to `docker inspect` on the host) |

The worker removes all `CLOUDSOURCED_*` variables from its environment
before it starts sessions.

## Sessions

* Each session runs in its own mount namespace and cgroup; build code never
  gets cloud credentials. The instance metadata service is unreachable from
  containers (AWS: IMDSv2 with hop limit 1; OCI: the supervisor unit rejects
  forwarded traffic to 169.254.0.0/16). Sessions do not get `ptrace` or a
  Docker socket.
* Only allowlisted environment variables (`[tool.X] env`) are forwarded;
  `PATH` and `HOME` come from the remote environment. Command arguments are
  forwarded as typed.
* `[secrets]` are resolved on the laptop per run and passed as environment
  variables of that run only; they are never stored remotely.
* Object store access is granted per lease: credentials scoped to the
  repository's and the environment's prefix, at most 12 h (STS session
  policy on aws-ec2 and with `[storage] role_arn`; otherwise the configured
  keys as they are, so scope them to the bucket). See
  [caches-and-storage.md](caches-and-storage.md#object-store-storage).
* Sessions of different repositories may share a machine (and its kernel)
  when they share an environment. Machines are a build sandbox, not a
  multi-tenant security boundary: run one pool per trust domain.
* local-docker machines need `CAP_SYS_ADMIN` and `/dev/fuse` for FUSE,
  per-session mount namespaces and cgroups. Access to the Docker socket is
  root on that host.

## Cloud permissions

* AWS: `deploy/aws/iam.yaml` creates least-privilege operator policies
  limited to stacks named `StackPrefix*`, instances tagged
  `project=cloudsourced` launched with IMDSv2 and hop limit 1, the
  `cloudsourced*` bucket and repository and SSM parameters under
  `/cloudsourced/`. Do not use root or admin credentials. See the
  [AWS guide](guides/aws.md).
* OCI: a compartment of its own, a dynamic group of its instances and a
  policy that only allows instance and volume management in the
  compartment, objects of the one bucket and reading repositories. See the
  [OCI guide](guides/oci.md).
* Machines accept QUIC only from `allow_cidrs` (default: the orchestrator's
  own public IP); there is no SSH ingress on cloud machines.

## Telemetry and logs

Traces record only the subcommand and flag names of a tool's arguments,
never values, positional arguments, environment or tokens. Agent detection
reads only the names and ids of known agent variables. Workers do not pass
`OTEL_*` variables on to sessions.
