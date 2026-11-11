# Guide: AWS EC2

Build machines are on-demand EC2 instances (Amazon Linux 2023) created by
the orchestrator, running the supervisor and the worker container. Shared
resources live in one CloudFormation stack that the provider creates on
first use.

## Prerequisites

* An AWS account and the AWS CLI v2, with credentials of an IAM user or SSO
  role that may create IAM resources once (for step 1).
* On the orchestrator host (your laptop in embedded mode, or the control
  plane): Docker with buildx, to build and push environment images. For
  arm64 sizes on an amd64 host additionally QEMU binfmt handlers
  (`docker run --privileged --rm tonistiigi/binfmt:qemu-v9.2.2 --install
  arm64`; emulated builds are several times slower).
* `make build`: it puts `bin/offshore-linux-arm64` (or `-amd64`) next to
  `bin/offshore`; the provider uploads the binary matching each machine.
* UDP 7443 from your laptop to the internet (QUIC to the machines).

## 1. Operator identity (`deploy/aws/iam.yaml`)

Do not use root or admin credentials. The template creates two managed
policies, `offshore-operator` (machines, images, artifacts, machine
secrets, disk growth, worker store grants) and `offshore-operator-stack`
(resources of the shared stack), and a role `offshore-operator` with both
attached.

```sh
aws cloudformation deploy --stack-name offshore-iam \
  --template-file deploy/aws/iam.yaml --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides TrustedPrincipalArn=arn:aws:iam::123456789012:user/alice RequireMfa=true
```

| parameter | default | |
|---|---|---|
| `StackPrefix` | `offshore` | name (prefix) of the provider's shared stack (provider option `stack`); bucket and repository names derive from it |
| `TrustedPrincipalArn` | any principal of the account allowed to assume it | who may assume the role: an IAM user, an SSO role, or the control plane's instance role |
| `RequireMfa` | `false` | require MFA when assuming the role |
| `CreateUser` | `false` | also create an IAM user `offshore-operator` for hosts that cannot assume roles (create its access key yourself) |

Outputs: `PolicyArn` and `StackPolicyArn` (exported as
`offshore-operator-policy` and `offshore-operator-stack-policy`),
`RoleArn`, `UserName`. The policies only cover stacks named
`StackPrefix*`, instances tagged `project=offshore` launched with IMDSv2
and hop limit 1, the `offshore*` bucket and repository and SSM parameters
under `/offshore/`. Once the shared stack exists, day-to-day operation only
needs `offshore-operator`.

```ini
# ~/.aws/config
[profile offshore]
source_profile = alice          # your own credentials
role_arn       = arn:aws:iam::123456789012:role/offshore-operator
mfa_serial     = arn:aws:iam::123456789012:mfa/alice   # with RequireMfa=true
region         = eu-central-1
```

A control plane on EC2 trusts its instance role
(`TrustedPrincipalArn=<instance role ARN>`) and assumes the operator role,
or gets the two policies attached directly. `deploy/aws/controlplane.yaml`
([control-plane.md](../control-plane.md#aws-deployawscontrolplaneyaml))
writes equivalent permissions into the control plane's instance role.

## 2. Quotas

New accounts have a small quota for on-demand standard instances
(A, C, D, H, I, M, R, T, Z families; default 5 vCPUs, quota code
`L-1216C47A`), which allows one 4-vCPU machine at a time:

```sh
aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A --profile offshore
aws service-quotas request-service-quota-increase --service-code ec2 \
  --quota-code L-1216C47A --desired-value 64
```

Set `pool.max_machines` (or the provider's `max_machines`) so that
`max_machines × vCPUs of the largest size` stays below the quota; a quota
error makes the scheduler take the provider out of scale-out for a cooldown
(2 minutes, doubling up to 30) instead of retrying in a loop.

## 3. Pool configuration

In the client file (embedded mode) or the control plane's pool file:

```toml
[pool]
max_machines = 4
idle_stop    = "20m"
max_lifetime = "24h"

[scheduler]
cost_optimization = 0.5              # 1 = cheapest, 0 = fastest result

[providers.aws]
type           = "aws-ec2"
profile        = "offshore"          # from step 1
region         = "eu-central-1"
allow_cidrs    = ["203.0.113.4/32"]  # clients that may reach machines (UDP 7443); required with a shared control plane
disk_gb        = 100                 # gp3 root volume
docker_command = ["docker"]

[[providers.aws.size]]
name           = "x4"                # a name of your choice
cpu            = 4
memory         = "7G"
disk           = "100G"
price_per_hour = 0.18                # your region's on-demand price
instance_types = ["c7a.xlarge", "c6a.xlarge", "m7a.xlarge"]   # tried in order

[[providers.aws.size]]
name           = "x8"
cpu            = 8
memory         = "15G"
disk           = "100G"
price_per_hour = 0.36
instance_types = ["c7a.2xlarge", "c6a.2xlarge"]
```

All options are listed in [configuration.md](../configuration.md#aws-ec2).
The `price_per_hour` values above are placeholders: enter the on-demand
prices of your region (AWS pricing pages or `aws pricing get-products`).
The scheduler's cost model and `offshore report` use them.

### Instance types and arm64

The instance types of a size are tried in order (default: the size name);
a capacity error (`InsufficientInstanceCapacity`, vCPU quota, type not
offered in the zone) moves on to the next type. Memory in a size should be
a little below the instance's nominal memory (the OS needs some).

```toml
[[providers.aws.size]]
name           = "g4"
arch           = "arm64"              # default "amd64"
cpu            = 4
memory         = "7G"
price_per_hour = 0.15
instance_types = ["c7g.xlarge", "m7g.xlarge"]
```

`arch = "arm64"` sizes (Graviton) boot the arm64 Amazon Linux 2023 AMI and
run `bin/offshore-linux-arm64` (options `binary_arm64`, `ami_arm64`
override). Environments are per architecture: a repository opts in with
`[resources] arch = "aarch64"`, and only sizes of that architecture are
created for it. Build outputs and test binaries must then run on an arm64
client, otherwise the test split falls back to running tests remotely.

## 4. First run

```sh
offshore config check                  # validates client, pool and repository files
offshore doctor                        # credentials, pool, daemon, environment image
OFFSHORE_REQUIRE_REMOTE=1 cargo build  # in a repository with .offshore.toml
offshore why                           # machine, provider, size, timings
```

On first use the provider creates the CloudFormation stack `offshore`
(`internal/provider/awsec2/stack.yaml`, needs `offshore-operator-stack`):

* a security group (ingress UDP 7443 from `allow_cidrs`, managed by the
  provider; no SSH);
* the machine role and instance profile (Session Manager, image pull,
  worker binary, its own secret parameter, stopping itself when idle);
* `StoreRole`: assumed by the orchestrator to mint short-lived worker
  credentials for `cache/*` in the bucket; every grant narrows it to the
  repository's and the environment's prefix with a session policy. This is
  the native object store for global caches (sccache, Go, ccache) and slot
  snapshots ([caches-and-storage.md](../caches-and-storage.md#object-store-storage));
* an S3 bucket (worker binaries below `bin/`, expired after 30 days; cache
  objects below `cache/`, expired after a day);
* an ECR repository for environment images (images expire after 30 days);
* a reaper Lambda that every 10 minutes terminates machines past their
  expiry and deletes orphaned machine secrets, even when no orchestrator
  runs;
* a resource group of everything tagged `project=offshore`.

Machines get their secrets through an SSM SecureString parameter
`/offshore/<stack>/machines/<id>`, which the supervisor reads once and
deletes. They terminate themselves at `max_lifetime` and stop themselves
when idle for `idle_stop` while no orchestrator has probed them for
`max(idle_stop, 10m)`. The scheduler can grow a full gp3 volume
(`ec2:ModifyVolume`, at most every six hours).

Cache volumes (provider part `Volumes`, see
[caches and storage](../caches-and-storage.md#volumes)) are encrypted gp3
EBS volumes tagged `project=offshore`, `offshore.pool` and
`offshore.volume`. The control plane needs `ec2:CreateVolume` (with
`aws:RequestTag/project = offshore`), `ec2:AttachVolume`,
`ec2:DetachVolume`, `ec2:ModifyVolume`, `ec2:DeleteVolume`,
`ec2:CreateTags`/`ec2:DeleteTags` (with `aws:ResourceTag/project =
offshore`, on volumes and instances) and `ec2:DescribeVolumes`;
`deploy/aws/iam.yaml` and `deploy/aws/controlplane.yaml` grant them. A
volume lives in one availability zone and attaches only to instances
there; a machine created with a volume is placed in its zone (the zone's
default subnet, or the configured `subnet` if it is in that zone).

## Costs

* **Instances**: billed per second while running (at least 60 s per start).
  Idle machines stop after `idle_stop`; stopped instances cost nothing for
  compute.
* **EBS**: gp3 root volumes are billed while the instance exists, also when
  stopped, until the machine is destroyed at `max_lifetime`.
* **S3, ECR**: small; cache objects live one day, binaries and images 30
  days.
* **Data transfer**: outputs and test binaries copied to your laptop are
  internet egress; the output overlay downloads only what you open.
  Restoring snapshots across providers is egress too (priced by
  `[[scheduler.egress]]`).
* **Control plane**: `deploy/aws/controlplane.yaml` runs one `t3.small` with
  a 20 GB state volume and an Elastic IP.

`offshore status` shows today's estimated cost, `offshore report --since 7d`
cost per day, repository, size and label. Lower `cost_optimization`
towards 0 when waiting is more expensive than machines, raise it towards 1
for cost.

## Cleanup

```sh
offshore down                                    # destroy machines and caches of the pool
bucket=$(aws cloudformation describe-stack-resource --stack-name offshore \
  --logical-resource-id ArtifactBucket --query StackResourceDetail.PhysicalResourceId --output text)
aws s3 rm "s3://$bucket" --recursive             # the bucket must be empty to be deleted
aws cloudformation delete-stack --stack-name offshore
aws cloudformation delete-stack --stack-name offshore-iam
```

The ECR repository is emptied by CloudFormation on deletion. A control
plane stack (`offshore-controlplane`) keeps a snapshot of its state volume
on deletion.

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `AccessDenied` / `UnauthorizedOperation` | the profile does not use the operator role, or the stack name does not match `StackPrefix*`; creating or updating the shared stack needs `offshore-operator-stack` |
| `VcpuLimitExceeded` | vCPU quota (step 2); lower `max_machines` or request more |
| `InsufficientInstanceCapacity`, `Unsupported` | list more `instance_types` for the size; the provider is put on cooldown when all fail |
| run hangs at connect, then falls back locally | UDP 7443 blocked, or the client's address is not in `allow_cidrs`. A shared control plane requires `allow_cidrs` (it will not start without it) and reconciles the security group to exactly that list; an embedded orchestrator without it adds its own public IP when it starts and keeps other rules |
| `ssm:PutParameter` denied | the machine secret channel needs SSM under `/offshore/`; use the operator role |
| arm64 image build fails with `exec format error` | install QEMU binfmt handlers or build on an arm64 host |
| stack in `ROLLBACK_COMPLETE` | delete it (`aws cloudformation delete-stack --stack-name offshore`) and retry; the events show the missing permission |
| builds always cold | check `offshore why` (warm slot, restored) and that a global cache is configured; see the [Rust guide](rust.md) |

`offshore doctor` and the daemon log (`~/.local/state/offshore/daemon.log`,
or the control plane's log) name the failing step; `offshore top` shows
machines starting and the queue.
