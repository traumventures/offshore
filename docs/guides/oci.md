# Guide: Oracle Cloud Infrastructure (OCI)

Build machines are OCI compute instances (Oracle Linux 9, flexible shapes)
running Docker CE, the supervisor and the worker container. The provider
creates its compartment, network, bucket, registry repository and IAM
policy on first use, idempotently.

## Prerequisites

* An OCI tenancy and an API signing key for a user, in `~/.oci/config`:

  ```ini
  [DEFAULT]
  user        = ocid1.user.oc1..aaaa...
  fingerprint = 12:34:...
  key_file    = ~/.oci/oci_api_key.pem
  tenancy     = ocid1.tenancy.oc1..aaaa...
  region      = eu-frankfurt-1
  ```

  (`oci setup config` creates it; upload the public key in the console
  under the user's API keys.)
* On the orchestrator host: Docker with buildx. Images are pushed to OCIR
  with a registry token of the API key (`docker login -u BEARER_TOKEN`,
  done by the provider); machines pull with a token of their instance
  principal.
* `make build` (also `bin/offshore-linux-arm64` for Ampere sizes).
* UDP 7443 from the clients to the internet.

## 1. Permissions

What the API user needs follows from what the provider creates: the
compartment (below the tenancy root, unless `compartment` is an existing
OCID), a VCN with internet gateway, route table, subnet and security list,
instances and boot volumes, an Object Storage bucket with lifecycle rules,
an OCIR repository, and, unless `iam = false`, a dynamic group and a policy
in the tenancy. A starting point for an operators group (adjust to your
tenancy's conventions):

```text
Allow group OffshoreOperators to manage compartments in tenancy
Allow group OffshoreOperators to manage dynamic-groups in tenancy
Allow group OffshoreOperators to manage policies in tenancy
Allow group OffshoreOperators to read objectstorage-namespaces in tenancy
Allow group OffshoreOperators to manage all-resources in compartment offshore
```

Cache volumes (provider part `Volumes`) are block volumes in the
compartment, attached paravirtualized at `/dev/oracleoci/oraclevdb`;
`manage all-resources` covers them (narrower: `manage volume-family` and
`use instance-family`). A volume lives in one availability domain and
attaches only to instances there; a machine created with a volume is
launched in its domain.

When the user may not manage IAM, set `iam = false` and let an
administrator create the dynamic group `offshore-<hash>` (matching rule
`ALL {instance.compartment.id = '<compartment OCID>'}`) and a policy with
these statements (from `policyStatements` in
`internal/provider/oci/infra.go`):

```text
Allow dynamic-group <group> to manage instance-family in compartment id <compartment OCID>
Allow dynamic-group <group> to manage volume-family in compartment id <compartment OCID>
Allow dynamic-group <group> to manage objects in compartment id <compartment OCID> where target.bucket.name = 'offshore'
Allow dynamic-group <group> to read repos in compartment id <compartment OCID>
Allow service objectstorage-<region> to manage object-family in compartment id <compartment OCID>
```

Machines can thus stop and terminate instances of the compartment
(themselves at idle stop and expiry), use the one bucket and pull images.
Keep the compartment for offshore only.

## 2. Pool configuration

```toml
[providers.oci]
type           = "oci"
profile        = "DEFAULT"            # ~/.oci/config profile (config_file overrides the path)
region         = "eu-frankfurt-1"     # default: the profile's region
compartment    = "offshore"           # name below the tenancy root (created) or an OCID
allow_cidrs    = ["203.0.113.4/32"]   # clients that may reach machines (UDP 7443); required with a shared control plane, reconciled (stale rules removed)
disk_gb        = 100                  # boot volume (min 50)
docker_command = ["docker"]

[[providers.oci.size]]
name           = "e5-8"
cpu            = 8                    # vCPUs: 4 OCPUs on E5.Flex
memory         = "32G"
price_per_hour = 0.20                 # your price per hour for this configuration
instance_types = ["VM.Standard.E5.Flex", "VM.Standard.E4.Flex"]   # tried in order

[[providers.oci.size]]
name           = "a1-8"
arch           = "arm64"
cpu            = 8                    # 8 OCPUs on A1.Flex
memory         = "48G"
price_per_hour = 0.10
```

All options are listed in [configuration.md](../configuration.md#oci).
Prices are placeholders; enter your tenancy's prices so the cost model and
`offshore report` are right.

### Shapes and OCPU math

Flexible shapes are configured from the size: OCPUs = `ceil(cpu / vCPUs per
OCPU)`, memory in GB rounded up. One OCPU is two vCPUs (a core with SMT) on
x86 shapes (E4/E5 Flex) and one core on Ampere A1. So `cpu = 8` gives 4
OCPUs on `VM.Standard.E5.Flex` and 8 OCPUs on `VM.Standard.A1.Flex`; use
even `cpu` values on x86 shapes. The memory must lie within the shape's
allowed range per OCPU. Without `instance_types` a size uses
`VM.Standard.E5.Flex` (amd64) or `VM.Standard.A1.Flex` (arm64). Fixed shapes
take no configuration. "Out of host capacity", service limit, quota and
unavailable-shape errors move on to the next shape; when all fail the
provider goes on cooldown.

### Store: the bucket, through pre-authenticated requests

The provider's bucket is the pool's native store (global caches such as
sccache, Go and ccache, and slot snapshots, so slots can move between
machines): objects live below `cache/<pool id>/`, which the bucket's
lifecycle rule deletes after a day. Workers get no credentials for it. With
every lease the control plane creates (or reuses) one **pre-authenticated
request per granted prefix** (`cache/<pool>/repos/<repo id>/`,
`cache/<pool>/envs/<env hash>/`): read and write for objects below that
prefix only, listing denied, valid for twice the grant time (24 h), and
reused while it outlives a new grant. Expired requests are deleted once an
hour. A new request takes a few seconds to reach every Object
Storage frontend (some answer `404 BucketNotFound` meanwhile); workers retry
such answers for about ten seconds. Pre-authenticated requests cannot delete objects; offshore never needs
to (expired objects are not served and the lifecycle rule removes them).
OCI's S3 compatibility API is not used: it needs a user's customer secret
keys, which cannot be scoped.

A `[storage]` section (any S3-compatible store, see
[caches-and-storage.md](../caches-and-storage.md#object-store-storage)) replaces the
native store.

## 3. First run

```sh
offshore config check
offshore doctor
OFFSHORE_REQUIRE_REMOTE=1 cargo build
offshore why
```

On first use the provider creates:

* the compartment;
* a VCN `offshore` (10.77.0.0/16) with internet gateway, default route and
  one public subnet (10.77.0.0/20) whose security list admits only UDP 7443
  from `allow_cidrs` (no SSH);
* a private bucket `offshore` (worker binaries, served to machines by a
  read-only pre-authenticated link; machine secrets below `secrets/`, read
  once by the supervisor with its instance principal and deleted; a
  lifecycle rule expires `cache/` and `secrets/` after a day; `cache/<pool>/`
  is the pool's store, see above);
* the OCIR repository `offshore-env` (registry
  `ocir.<region>.oci.oraclecloud.com` unless `registry` is set);
* the dynamic group and policy (step 1).

Containers cannot reach the metadata service (the supervisor unit rejects
forwarded traffic to 169.254.0.0/16). An OS shutdown only stops an OCI
instance, so at `max_lifetime` the supervisor terminates its instance
through the API (which deletes the boot volume); there is no reaper
function as on AWS.

## Limits

* Service limits per shape family and availability domain (cores, memory)
  cap the pool; check them in the console (Governance, Limits, Quotas and
  Usage) and size `max_machines` accordingly.
* Preemptible capacity is not supported.
* Ampere A1 (arm64) sizes are implemented but only x86 shapes were tested
  live ([ROADMAP.md](../ROADMAP.md)).
* Machines of one pool use one availability domain (option
  `availability_domain`, default the first).

## Cleanup

```sh
offshore down
```

then delete, in the compartment: remaining instances, the bucket (empty it
first), the OCIR repository, the VCN (subnet, route table, internet
gateway), and in the tenancy the dynamic group and policy `offshore-<hash>`,
finally the compartment. The OCI smoke test has a cleanup:
`go test -tags ocismoke -run TestOCICleanup -v ./internal/orchestrator`.

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `NotAuthorizedOrNotFound` while creating infrastructure | the API user lacks a permission from step 1; with `iam = false` an administrator must create the dynamic group and policy |
| `list dynamic groups` error | set `iam = false` and create the IAM part yourself |
| `Out of host capacity` / `LimitExceeded` | add shapes to `instance_types`, try another availability domain, raise service limits |
| `disk_gb must be at least 50` | the minimum boot volume is 50 GB |
| machine never becomes ready | the supervisor cannot read its secret or pull the image: check the dynamic group's matching rule and policy |
| run falls back locally at connect | UDP 7443 not allowed from the client (`allow_cidrs`) |
| every build on a new machine is cold | the store is unavailable: look for `store unavailable` / `store grant failed` in the orchestrator log (creating pre-authenticated requests needs `manage buckets` on the compartment) |

`offshore doctor`, `offshore top` and the daemon or control plane log show
the failing step.
