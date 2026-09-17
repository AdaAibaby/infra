# E2B Embed reference

The [hub README](../README.md) says what E2B Embed is and how to pick a
shape. This page holds the rest: what runs where, the secrets, how images and
pins are released, building templates, and the developer tooling. The 3
guides cover what differs per shape.

## What runs where

The 4 stores run in containers on a bridge network with their ports on
`127.0.0.1`. api, client-proxy, dashboard-api, the dashboard and the released
orchestrator run on the machine's own network. The orchestrator is a host
process, launched through `nsenter` by a privileged container, the same
pattern E2B's own Kubernetes deployment uses. Its launcher ends every sandbox
when it stops.

8 one-shots run before the stack is usable, in this order:

- `preflight` fails fast with a `FIX:` line when the machine is unsuitable.
- `api-secrets` generates the api's admin token and sandbox-token hash seed.
  It needs only the volume, so it finishes first.
- `host-setup` prepares the machine on every start.
- `fetch-artifacts` downloads and verifies the 5 Firecracker binaries.
- `db-migrator` and `clickhouse-migrator` bring the 2 databases to the schema
  their pinned images expect.
- `seed` writes the default team and generates this install's API key.
- `base-template` builds the default template through the API.

Kubernetes runs 7 of them: there the api's 2 secrets come from a Secret
instead. `ready` is the marker that everything above worked. It is a
long-running container, not a one-shot, and its readiness is the whole
stack's.

dashboard-api starts once `db-migrator` and `api-secrets` are done and reaches
the same stores as the api; the dashboard starts once api and dashboard-api
are healthy.

### Ports on loopback

Everything outside the hub's ports table stays on loopback. Postgres is on
5432, Redis on 6379, ClickHouse on 8123 and 9000. On Kubernetes ClickHouse
also listens on 9004, 9005 and 9009, its MySQL and PostgreSQL wire protocols
and its interserver port, since the pod shares the node's network. Vector's
log listener is on 30006 (20006 on Kubernetes, for the reason that guide
gives) and its API on 44313, on Kubernetes only. The 2 pprof endpoints are
6060 for api and 6061 for the orchestrator. `sudo ss -ltnp` on the machine
confirms the split: the 13 ports in the hub's table show a `*:` address,
everything here shows `127.0.0.1:`.

Port 5008 creates and kills sandboxes and starts template builds. Nothing
authenticates it; the api dials it directly as `LOCAL_ORCHESTRATOR_ADDRESS`,
and anyone who reaches it has the whole orchestrator. The 4 egress-proxy
ports (5010 and 5016 to 5018) take the sandbox traffic the orchestrator
redirects inside each sandbox's network namespace. They expect no client from
outside the machine and have no authentication of their own.

### Logs

Sandbox and template-build logs live in ClickHouse. The orchestrator and the
api ship their lines to Vector: envd's output from inside the VM, the
orchestrator's per-sandbox events, the template-manager's build output and
the api's own. Vector turns each into a row of the `sandbox_logs` table. The
api reads that table for the SDK's `getLogs` and `e2b template build`.
`LOGS_READ_CONFIG=true` sets the api's `logs-read-config` flag, since there
is no LaunchDarkly here to set it. Retention is the table's 7 days. There is
no Loki in this stack, and the api needs no `LOKI_URL`.

## Secrets

The team API key is per install. The seed generates it on the first start and
keeps it beside the databases, so a shape never has a key without its
database or a database without its key. Every shape prints it with the 2 SDK
URLs and the dashboard URL. Each guide's Secrets section says where its copy
lives and how to pin or rotate it. A rotation revokes the old key, which keeps
working for up to 5 minutes: the api caches team lookups in Redis for that
long.

The dashboard takes that same key in its key form and keeps it in an httpOnly
browser cookie for a year; sign-out clears it, and rotating the key signs
every browser out. Embed serves the dashboard over plain http, so the cookie
is not marked Secure (`DASHBOARD_COOKIE_SECURE=false`); a browser on a
plain-http address would otherwise drop it and the key form would loop.

`ADMIN_TOKEN` and `SANDBOX_ACCESS_TOKEN_HASH_SEED` are the api's own 2, and
every shape generates them per install too. Compose writes them on the first
start into the volume that holds the team key (`/run/e2b/api.env` in
`seed-state`). Terraform writes generated ones into the instance's `.env` at
first boot. Kubernetes reads the Secret the install creates. On Compose,
setting either one in `.env` pins it and leaves the other generated; each
guide's Secrets section says how to rotate what its shape holds. dashboard-api
reads the admin token the same way on each shape, so rotating it means
recreating dashboard-api as well as the api.

Both are worth guarding. The admin token is admin over the seeded team on
port 3000 without the team API key, including minting and revoking API keys;
the team's id is a public constant. The hash seed makes the traffic and envd
tokens of `secure` sandboxes computable from a sandbox ID by anyone who
reaches 3002.

## Images and pins

[`compose/.env`](../compose/.env) is the source of truth for every E2B
version the stack uses: the 5 released E2B service images (api, db-migrator,
dashboard-api, client-proxy, clickhouse-migrator), the dashboard image, the 3
stack images Embed builds itself, and the 5 Firecracker binaries. The 4 store
images are pinned inline in [`compose/compose.yaml`](../compose/compose.yaml)
and repeated in the StatefulSet. The stack images carry everything that is not
a released E2B service: the host scripts, the SDK scripts and the database
seeder. Terraform ships the `.env` to the instance, and Kubernetes repeats its
pins in [`kubernetes/kustomization.yaml`](../kubernetes/kustomization.yaml).
The 3 stack images live in the `embed` repository of the `e2b-artifacts`
registry.

Embed is released together with the platform at one version. That release
moves every platform pin in both files to it: api, db-migrator,
dashboard-api, client-proxy, clickhouse-migrator, the orchestrator and the 3
stack images, the lines carrying a release marker. A checkout at a release
therefore names one version everywhere and pulls exactly what that release
published; see [RELEASING.md](../../docs/RELEASING.md). envd has its own
release line, and the kernel, Firecracker and BusyBox are not released here,
so those 4 are pinned by hand. A bump adds the binary's checksum to
[`compose/scripts/fetch-artifacts.sh`](../compose/scripts/fetch-artifacts.sh)
first. The orchestrator needs no row: each release writes a `.sha256` beside
the binary it publishes, and `fetch-artifacts` verifies against it. The
dashboard image is released from its own repository
(github.com/e2b-dev/dashboard, tags `vX.Y.Z`) and is pinned by hand too; its
line carries no release marker. All of it is public and pulled anonymously;
the stores come from Docker Hub.

The 9 pinned images are published for both architectures; `fetch-artifacts`
verifies the arm64 orchestrator and envd against the `.sha256` their release
writes beside the object; a pin with no such
object stops with a `FIX:` line naming it.

To pin an install, pin the commit. The Compose files come from raw URLs, so
put the commit in place of `main` in their path. The Terraform `source` and
the `kubectl apply -k` URL are git URLs and take `?ref=<commit>`. The `main`
URLs the guides use give the newest.

![Layer map: source files, build definitions, images, services](layer-map.svg)

Top to bottom: the source files, the Dockerfiles that copy them, the images
(3 built here, 10 pulled ready-made) and the compose services. Arrows in the
last band are `depends_on` gates, in start order. The stripe on each service
says which image it runs. 2 things the picture leaves out: the ClickHouse and
Vector configs are inlined into [`compose/compose.yaml`](../compose/compose.yaml)
rather than shipped in an image, and the 5 Firecracker artifacts never enter
an image at all. `fetch-artifacts` writes them onto the machine and the
orchestrator reads them there.

## Beyond the first sandbox

`Template.build` builds your own template through the same API, on any shape:

```python
from e2b import Template, Sandbox
tpl = Template().from_python_image("3.12").run_cmd("pip install requests")
Template.build(tpl, alias="py-requests", cpu_count=2, memory_mb=1024, on_build_logs=lambda e: print(str(e)))
sbx = Sandbox.create("py-requests")
print(sbx.commands.run("python3 -c 'import requests; print(requests.__version__)'").stdout)
sbx.kill()
```

The build runs inside a Firecracker VM on the machine and pulls the image
from Docker Hub; no Docker daemon is involved. How long it takes depends on
how much of the base image is already cached.

`sandbox.get_host(port)` returns `{port}-{id}.e2b.app`, which does not resolve
here. Reach a port inside a sandbox through client-proxy's header routing
instead:

```bash
curl -H "E2b-Sandbox-Id: $SANDBOX_ID" -H "E2b-Sandbox-Port: 8080" http://localhost:3002/
```

The dashboard at `http://localhost:3001`, the `E2B_DASHBOARD_URL` that
`ready` prints, shows the same sandboxes and templates: paste `$E2B_API_KEY`
into its key form. Its terminal and filesystem inspector reach a sandbox
through the same header routing, at the address in `E2B_DASHBOARD_HOST`
(default `localhost`). Terraform writes it into the instance's `.env`, and
the Kubernetes manifest sets the same address from the node's IP; on Compose
set it in `.env` when a browser on another machine opens the dashboard
without a tunnel.

## Developing

`make` is a developer convenience; the operator path is only `docker compose`,
`terraform` or `kubectl`.

| Target | What it does |
|--------|--------------|
| `make images` | build the 3 stack images locally under their pinned tags |
| `make lint` | render the compose file and the kustomization, validate the Vector config and the Terraform module, shellcheck the scripts and the tests |
| `make test` | run the bats suite in `tests/` |
| `make stores-check` | the store-level integration check |
| `make sync-configs` | re-inline the 2 configs into the compose file |

### What the targets need

`make lint` wants `shellcheck`, `terraform` (1.7.5 or newer) and `kubectl`,
for `kubectl kustomize`. `terraform init` downloads the google and random
providers, so it needs network. `make test` wants `bats` plus `kubectl`,
which `tests/kubernetes.bats` renders the manifest with.
`tests/terraform.bats` reads the module's files as text and needs no
terraform.

Both also need a working Docker daemon with the compose plugin, and `jq` on
`PATH`. `make lint` renders `compose/compose.yaml` and validates the Vector
config with the Vector image (`make vector-validate`).
`tests/inline-configs.bats` diffs the rendered inline configs against the
copies under `compose/config/`. `tests/vector-rows.bats` replays the fixture
log lines in `tests/fixtures/vector/` through the shipped Vector config with
that image, with `compose/scripts/dev/vector-testconfig.py` swapping only the
source for stdin and the sink for a JSON console, and asserts the
`sandbox_logs` row each becomes or that it is dropped. It needs Docker,
python3 and `jq` as well. `tests/pins.bats` keeps the 3 pins, the 2 dashboard
pins, their Kubernetes counterparts and the registry the images are built
into in step with each other.

### Inline configs

After editing `compose/config/vector/vector.toml` or
`compose/config/clickhouse/config.xml`, run `make sync-configs` (python3). It
rewrites the inline copies in `compose/compose.yaml` and the
`VECTOR_CONFIG_SHA256` and `CLICKHOUSE_CONFIG_SHA256` stamps. The stamps make
Compose recreate the container on a config change, which it does not do for
inline `configs:` content on its own. `tests/config-hashes.bats` fails until
the stamps match.

### The stores check

`make stores-check` runs what CI's stores job runs: both stores, their
migrators, the seed, and the assertions that the seed wrote the team API key
file, that the seeded row hashes that key, and that both migration ledgers
landed. It wants Docker, network access and the images from `make images`,
and needs no KVM. It deliberately leaves the containers up so a failure can
be inspected, so finish with
`docker compose --project-directory compose down -v`.

### Rebuilding the stack images

The scripts under `compose/scripts/` and `compose/scripts/node/` travel inside
the 2 stack images that carry them, and the compose file mounts nothing from
the repository. After editing either directory, run `make images` before the
next start, or the stack silently keeps running the old code. Without `make`,
this is the `images` target (its `$$` becomes a single `$` outside of make):

```bash
set -a; . ./compose/.env; set +a
docker buildx bake --load \
  --set "*.platform=linux/$(docker version -f '{{.Server.Arch}}')" \
  --set "seed.context=https://github.com/e2b-dev/runtime.git#${RUNTIME_COMMIT}" \
  --set seed.args.SRC=packages \
  --set "tools.tags=${E2B_TOOLS_IMAGE}" \
  --set "node-e2b.tags=${E2B_NODE_E2B_IMAGE}" \
  --set "seed.tags=${E2B_SEED_IMAGE}"
```

Binary checksums live in the tools image's `compose/scripts/fetch-artifacts.sh`,
so bumping a Firecracker artifact means new stack images, not an edit on a
machine.
