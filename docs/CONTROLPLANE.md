# Running the control plane

`offshore controlplane` serves the pool API (leases, environments,
status) over HTTPS with one global API key. Clients set `[control_plane]

The control plane works **without a domain**: it serves a persistent
self-signed certificate that clients pin by fingerprint. A domain is **only a
question of the hostname**: `--hostname cs.example.com` (a DNS name pointing at
the control plane) makes it obtain a Let's Encrypt certificate, and clients
drop the fingerprint.

## Flags and commands

```
offshore controlplane [flags]                  serve
offshore controlplane info [--show-key]        URLs, TLS mode, fingerprint, API key location
offshore controlplane client-config [--url U]  print the client [control_plane] block
offshore controlplane health                   liveness check (container HEALTHCHECK)
```

| flag | default | |
|---|---|---|
| `--hostname` | `$OFFSHORE_HOSTNAME` | public DNS name; turns on ACME. With `--acme=false` (or an IP address) it is added as SAN to the self-signed certificate instead |
| `--acme` | true iff `--hostname` is a DNS name | Let's Encrypt via `autocert`; certificate and account cached in `<state>/acme` |
| `--acme-email` | `$OFFSHORE_ACME_EMAIL` | contact address of the ACME account |
| `--acme-http` | `:80` with ACME | HTTP-01 challenge listener (other requests redirect to https); `off` = TLS-ALPN-01 on the API port only (then 443 must be reachable by Let's Encrypt) |
| `--acme-directory` | Let's Encrypt production | e.g. the staging directory for tests |
| `--listen` | `:7443`, `:443` with ACME | `$OFFSHORE_LISTEN` |
| `--tls-cert/--tls-key` | | own certificate instead |
| `--insecure-http` | | plain HTTP behind a TLS terminating proxy |
| `--api-key-file` | `$OFFSHORE_API_KEY`, else generated `<state>/api-key` | the key is never logged; `info --show-key` prints it |
| `--state-dir` | `$OFFSHORE_STATE_DIR` or `~/.local/state/offshore/controlplane` | |
| `--machine-binary` | see below | worker binary for local-docker machines, as a path on the Docker host |
| `--config` | `$OFFSHORE_POOL_CONFIG`, else `$OFFSHORE_CONFIG`/`~/.config/offshore/config.toml` | pool configuration (`[pool]`, `[providers.*]`); `[control_plane]` and `user` in it are ignored with a warning |

The pool configuration is reloaded when the file changes (checked before
each lease and every 15 s), on `SIGHUP` and on `offshore reconcile` from a
client; an invalid file is logged and the previous configuration stays in
effect. `pool.id`, the providers and `pool.estimator` apply only after a
restart. Repository settings (build and dependency cache TTLs) come with each
request and never change the pool configuration. `offshore config check
--pool FILE` validates a pool file before deploying it.

The state directory holds `api-key`, `tls.json` (self-signed certificate,
created once, so the fingerprint survives restarts; replaced only when a new
`--hostname` is not covered by it), `acme/`, uploaded environment contexts,
usage history and `controlplane.json` (what `info`, `client-config` and
`health` read; no secrets). SIGTERM/SIGINT shut down gracefully (running
requests get 20 s). `/healthz` needs no API key.

Clients:

```sh
offshore controlplane client-config                       # on the control plane host
offshore controlplane client-config --url https://1.2.3.4:7443   # anywhere: fetches the fingerprint
```

```toml
[control_plane]
url = "https://10.0.0.5:7443"
fingerprint = "0b91…"                # only with the self-signed certificate
api_key = "env:OFFSHORE_API_KEY" # or file:…, cmd:…
```

When `client-config` fetches the fingerprint from a remote URL (trust on first
use) compare it with `offshore controlplane info` on the server.

## From no domain to a domain

1. Point a DNS name at the control plane (A record to its public IP).
2. Restart it with `--hostname cs.example.com` (and optionally `--acme-email`),
   or set `OFFSHORE_HOSTNAME`. Ports 80 (HTTP-01) and 443 must be open;
   the API moves from 7443 to 443.
3. Clients: `url = "https://cs.example.com"`, delete the `fingerprint` line
   (`offshore controlplane client-config` prints the new block). The API
   key stays the same.

On AWS (below) these steps are a stack update: `Hostname=cs.example.com` and,
with Route 53, `HostedZoneId=Z…`.

## Container image

```sh
make image DOCKER="sudo -n docker"     # offshore-controlplane:<version>, :latest
```

`Dockerfile` builds the static linux/amd64 binary (amd64 machines get the
control plane's own binary, arm64 machines `/usr/local/bin/offshore-linux-arm64`,
built alongside) and puts it on `docker:cli` (Docker CLI with
buildx, which the providers use to build environment images, plus CA
certificates). It runs as uid 10001 with `cap_net_bind_service` on the binary
(for 443/80), state in `/var/lib/offshore` (also `$HOME`, so the Docker
CLI's registry logins persist), config `/var/lib/offshore/config.toml`.

### Environment images

The control plane builds environment images (`internal/image`): once per
environment hash and platform, with the Docker CLI of the provider that
needs it (local-docker `command`, aws-ec2 `docker_command`), reusing an
existing image and Docker's layer cache. Registries receive images lazily:

- local-docker machines run the image from the local daemon; it is built
  eagerly when a client registers the environment, so the first build is
  reported to the user right away;
- aws-ec2 pushes to the stack's ECR repository (`<repo>:<hash>-<arch>`) right
  before the first instance of that environment is launched; the build and
  push are reported to the clients waiting for that machine.

The registry is asked first (ECR `DescribeImages`): when it already holds
the image, nothing is built and Docker is not called, so a control plane
that only serves aws-ec2 and finds its images in ECR does not need Docker.
Building a new environment does need a Docker CLI on the control plane (a
remote builder is a possible extension).

Access to the host's Docker socket is equivalent to root on the host; the
container is unprivileged otherwise. It needs the socket's group:
`--group-add $(stat -c %g /var/run/docker.sock)`.

### Local Docker host (local-docker provider)

```sh
sudo install -d -o 10001 -g 10001 -m 0700 /srv/offshore
sudo cp pool-config.toml /srv/offshore/config.toml   # [providers.x] type = "local-docker", command = ["docker"]
sudo chown 10001:10001 /srv/offshore/config.toml
docker run -d --name offshore-controlplane --restart unless-stopped \
  --network host \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/offshore:/var/lib/offshore \
  offshore-controlplane:latest            # controlplane [flags]
docker exec offshore-controlplane offshore controlplane client-config
docker exec offshore-controlplane offshore controlplane info --show-key
```

Why `--network host`: local-docker machines publish their QUIC port on the
host (`host` option, default 127.0.0.1); the control plane probes them there
and hands that address to clients. With host networking 127.0.0.1 is the host.
(With `aws-ec2` only, `-p 7443:7443` works as well.)

Why the binary needs care: local-docker bind mounts the worker binary into
every machine, and the Docker daemon resolves that path **on the host**. In a
container (detected by `/.dockerenv` or `OFFSHORE_IN_CONTAINER=1`, set by
the image) the control plane therefore copies its own executable to
`<state>/bin/offshore` and finds that file's host path from its own
mounts (`docker inspect` of its container id from `/proc/self/mountinfo`), so
bind mounts and named volumes both work. Explicit alternatives: the provider
option `binary = "/host/path"` or `--machine-binary /host/path`; if the
lookup fails it assumes the state directory is mounted at the same path.
`deploy/docker/smoke-test.sh` exercises exactly this setup with a real build.

Containers, volumes and images of machines are created on the host Docker, as
with a native control plane; `offshore down` removes them.

## AWS (deploy/aws/controlplane.yaml)

One `t3.small` (x86_64, Amazon Linux 2023, IMDSv2 only, encrypted root disk)
runs the image with systemd (`offshore-controlplane.service`, host
networking, host Docker for environment image builds). Resources:

- Elastic IP; security group with the API port from `ClientCidr` (7443, or
  443 with a hostname) and, only with `Hostname`, port 80 from anywhere for
  ACME HTTP-01 (everything else on 80 redirects to https).
- Encrypted gp3 state volume at `/var/lib/offshore` (snapshot on delete
  and replacement): API key, certificate, ACME cache, contexts, history.
- ECR repository `<stack>-image` for the control plane image (unless
  `ImageUri` is given); the service retries until the image is pushed.
- SSM parameter `/<stack>/controlplane.env` (hostname, ACME e-mail, image):
  read at every start and polled every minute, so a stack update of
  `Hostname` takes effect without replacing the instance.
- Optional Route 53 A record when `Hostname` and `HostedZoneId` are set.
- Instance role: Session Manager (operator access, no SSH), pull of the
  image, and the **aws-ec2 operator permissions**: run/start/stop/terminate
  instances tagged `project=offshore`, ingress on the pool security
  group, the binary bucket and environment repository, the AMI parameter,
  machine secrets under `/offshore/<PoolStackName>/` (P1), and creating
  and updating the pool stack `PoolStackName` (CloudFormation acts with the
  caller's permissions), limited to resources named after that stack. The
  policy is written here; the dedicated IAM template of workpack P4 may
  replace it.
- `config.toml` is written to the state volume on first boot (aws-ec2
  provider with `region`, `stack`, `subnet`, `allow_cidrs = [ClientCidr]`,
  one size from `MachineInstanceType/Cpu/Memory`); edit it there afterwards
  (it is re-read on every scheduling call).

Deploy:

```sh
aws cloudformation deploy --stack-name offshore-controlplane \
  --template-file deploy/aws/controlplane.yaml --capabilities CAPABILITY_IAM \
  --parameter-overrides VpcId=vpc-… SubnetId=subnet-… ClientCidr=203.0.113.4/32
repo=$(aws cloudformation describe-stacks --stack-name offshore-controlplane \
  --query "Stacks[0].Outputs[?OutputKey=='RepositoryUri'].OutputValue" --output text)
aws ecr get-login-password | docker login --username AWS --password-stdin "${repo%%/*}"
make image-push IMAGE_REPO="$repo"
# client block and API key (outputs ClientConfigCommand, then get-command-invocation):
aws ssm send-command --instance-ids i-… --document-name AWS-RunShellScript \
  --parameters 'commands=["docker exec offshore-controlplane offshore controlplane client-config --url https://<PublicIp>:7443","docker exec offshore-controlplane offshore controlplane info --show-key"]'
```

Add the domain later:

```sh
aws cloudformation deploy --stack-name offshore-controlplane \
  --template-file deploy/aws/controlplane.yaml --capabilities CAPABILITY_IAM \
  --parameter-overrides Hostname=cs.example.com HostedZoneId=Z… AcmeEmail=ops@example.com
```

(without Route 53: omit `HostedZoneId` and point the name at output
`PublicIp`). Within a minute the instance restarts the container with
`OFFSHORE_HOSTNAME`, which obtains the certificate on the first request;
clients switch to `url = "https://cs.example.com"` without `fingerprint`.
The template has been checked with `aws cloudformation validate-template` and
`cfn-lint`, not deployed.
