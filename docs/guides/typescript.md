# Guide: TypeScript and Node (bun, npm, pnpm, yarn)

The `node` adapter handles `bun`, `bunx`, `npm`, `npx`, `pnpm` and `yarn`.
Installs, builds, scripts and tests run remotely; `node_modules` is a
machine cache generated on the machine from the lockfile, so sessions start
with dependencies in place and a local `node_modules` never travels.

## Prerequisites

* A configured pool ([README](../../README.md#quick-start-local-docker-no-cloud-account), [AWS](aws.md), [OCI](oci.md)).
* A lockfile: `bun.lock`/`bun.lockb`, `pnpm-lock.yaml`, `package-lock.json`
  or `yarn.lock`. Without one, `node_modules` is a plain machine cache that
  is not keyed by content.
* Shims for your package manager: `offshore shim install bun bunx` (or
  `npm npx`, `pnpm`, `yarn`).

## Setup

1. `offshore init` detects `package.json` and the package manager from the
   lockfile (preference: bun, pnpm, npm, yarn) and proposes the image
   (`oven/bun:1-debian` for bun, else `node:lts-trixie-slim`), the
   `node-modules` cache with the lockfile's install command and a
   `[tool.<manager>]` section. Write it with `offshore init --write`.
2. Decide which scripts run locally (see below) and list them in `local`.
3. `offshore doctor`, then `bun install` (or `npm ci`) and `offshore why`.

## Example configuration

A bun monorepo with the web app in `web/`:

```toml
version = 1

[resources]
cpu    = 4
memory = "4G"

[environment]
image = "oven/bun:1-debian"

[environment.packages]
apt = ["python3", "make", "g++"]   # native modules (node-gyp)

[cache.node-modules]
scope   = "machine"
paths   = ["web/node_modules"]
digest  = ["web/package.json", "web/bun.lock"]
install = "cd web && bun install --frozen-lockfile"

[cache.web-dist]                    # build output stays warm per slot
scope = "slot"
paths = ["web/dist"]

[tool.bun]
local = ["run dev"]                 # the dev server runs here
env   = ["NODE_ENV", "NODE_OPTIONS", "CI"]
eager = ["web/dist/**"]             # copy the build back after every run
ports = [3000]                      # or forward it while it runs remotely

[tool.bunx]
env = ["NODE_ENV"]
```

For npm use `node:lts-trixie-slim`, `digest = ["package.json",
"package-lock.json"]` and `install = "npm ci"`; for pnpm
`pnpm install --frozen-lockfile` (the image needs pnpm, e.g. a setup step
`corepack enable`); for yarn `yarn install --frozen-lockfile`.

## The `node_modules` machine cache

The digest covers the listed files (here `package.json` and the lockfile),
the cache's name, paths and install command, and the machine platform
(OS, architecture, C library). The first session that needs a digest runs
`install` on the machine and stores the result in the machine store; every
later session gets a copy linked from it (reflinks, or read-only hard
links). A changed lockfile produces a new digest; old ones are removed
after `ttl` (default 7 days) or beyond `max_size`. With an object store the
installed tree is saved per digest and other machines restore it instead of
installing again ([caches-and-storage.md](../caches-and-storage.md#machine-caches-per-digest-node_modules)).

Consequences:

* **Platform and native modules.** Native addons (esbuild, sharp, swc,
  ...) are installed for the machine's platform. A laptop on macOS or with
  another libc never shares binaries with the machine, and that is
  intended: the paths are hidden from the source projection, so the local
  `node_modules` stays local and the remote one stays remote.
* `bun install` / `npm ci` typed on the laptop run remotely and refresh
  the machine's copy; scripts that run locally need a local install of
  their own (`OFFSHORE_DISABLE=1 bun install`).
* Do not write to `node_modules` from scripts: with hard links it is
  read-only.

## Local or remote scripts and tests

Everything not listed in `local` runs remotely, including `test` and
`run <script>`. `local = ["test"]` keeps the test command local,
`"run dev"` one script. Typical choices:

| command | where | why |
|---|---|---|
| `bun install`, `npm ci`, `pnpm add` | remote | fills the machine cache |
| `bun run build`, `tsc`, lint | remote | CPU-heavy; outputs come back via `eager` or `offshore fetch` |
| `bun test`, `vitest run` | remote (or local) | remote by default; list `"test"` in `local` when tests need local services or a browser |
| `bun run dev`, `vite` | local, or remote with `ports` | dev servers need file watching and a browser; remote works with port forwarding |
| Playwright with a headed browser | local | needs a display |

There is no test split for node (no remote build, local run); tests run
entirely on one side.

## Verification

```sh
offshore doctor
bun install && offshore why    # adapter node, phase deps remote, digest install on first run
bun run build && ls web/dist   # eager outputs came back
offshore top --once
```

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `install: required with digest` | a digest cache needs an install command |
| install runs on every new machine | no object store (OCI/static without `[storage]`), or the lockfile changed |
| `EROFS` writing into `node_modules` | the copy is hard-linked read-only on file systems without reflinks; do not modify dependencies at run time |
| native module fails to load locally | you ran a local script against a remote install; install locally for local scripts |
| script output missing locally | add it to `eager`, or `offshore fetch 'web/dist/**'` |
| dev server unreachable | add the port to `[tool.X] ports` or run the script locally |
