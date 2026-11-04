# deploy

The one-command installer and the Docker Compose files.

| Path | Contents |
|---|---|
| `install.sh` | The installer: checks the machine, writes the bundle and its settings, starts Orglane and prints the URL |
| `compose/` | The production bundle: Traefik, the Orglane core and PostgreSQL, and the agent for managed services (off by default) |
| `compose/pebble/` | **Testing only:** an overlay that runs domain mode against Pebble, Let's Encrypt's ACME test server |
| `dev/` | Local development only: a PostgreSQL for `make dev` and the matching `orglane.yaml` |

## The installer

```sh
curl -fsSL https://orglane.app/install.sh | sh                        # asks: demo, domain or local
curl -fsSL https://orglane.app/install.sh | sh -s -- --mode demo --yes  # the demo on a laptop, no questions
curl -fsSL https://orglane.app/install.sh | sudo sh -s -- --mode domain \
    --domain acme.com --workspace-host work.acme.com --acme-email admin@acme.com --yes
```

`orglane.app/install.sh` serves this file unchanged, with its SHA-256 on the landing page (see [website/README.md](../website/README.md)). `install.sh` is POSIX sh (dash, busybox and bash). When it is piped into `sh`, it reads answers from the terminal (`/dev/tty`). With `--yes` it asks nothing and uses the options and defaults. `install.sh --help` lists every option.

| Mode | For | Result |
|---|---|---|
| `demo` | Trying Orglane on a laptop (Linux or macOS with Docker Desktop) | `http://localhost` with the "Acme Robotics" sample company and one-click sign-in as any of its people (local ingress mode, `ORGLANE_DEMO_ENABLED=true`) |
| `local` | An empty instance on this computer | `http://localhost` and a one-time setup token for creating the owner |
| `domain` | Production on a Linux server with a public IP | `https://<workspace host>` with a Let's Encrypt certificate and a one-time setup token |

What it does, in order:

1. Checks the OS and CPU architecture (Linux and macOS; amd64 and arm64), Docker Engine 25+ and Compose 2.24+. If Docker is missing on Linux, it offers the official convenience script (`https://get.docker.com`). It installs Docker only after you confirm, or with `--install-docker`, never because of `--yes` alone.
2. Checks CPUs, the memory Docker can use and the free disk space, and warns below the recommended size (domain mode: 2 CPUs, 4 GB, 10 GB disk).
3. Fetches the Compose bundle (`compose.yaml`, `modes/`, `pebble/`) of the requested release (`--version`, default: the latest GitHub release) from the source archive of its tag, and uses the image `ghcr.io/orglane/orglane:<version>`. `--source <checkout>` takes the bundle from a checkout instead, together with `--image` (for example `orglane:dev` from `make image`) or `--version`.
4. Domain mode only: checks DNS and reachability **before** anything requests a certificate (see below).
5. Generates the database password and the master secret with a CSPRNG (`openssl rand`, else `/dev/urandom`). It writes them with every other setting to `<data dir>/.env` (mode 600, data directory mode 700) and sizes PostgreSQL from the memory Docker reports (see [PostgreSQL sizing](#postgresql-sizing)).
6. Runs `docker compose up --detach --wait`, waits until the workspace answers `/readyz` through Traefik (in domain mode also until the certificate is issued), and prints the URL with a one-time setup token (`orglane cli setup-token`) or, in demo mode, the URL to open for one-click sign-in. The last line of output is the URL alone, for scripts.

The data directory (`--data-dir`, default `/var/lib/orglane` on Linux, which needs root, and `~/.orglane` on macOS) holds everything: the bundle, `.env` and the data (`postgres/`, `traefik/`). `docker compose` commands work from there, for example `cd /var/lib/orglane && docker compose logs -f`.

**Re-runs** detect the existing install and upgrade or repair it. The bundle and the image are replaced, while the secrets, the mode, the domain settings and the data are kept. Settings you add under `# --- Your settings` at the end of `.env` are kept too, and they override the ones above them. Changing the mode of an install is refused. Automatic rollback of a failed upgrade comes with the upgrade flow of [ADR 0007](../docs/adr/0007-distribution-upgrades-rollback.md).

```sh
sh install.sh status      # mode, version, URL, containers, readiness (exit 1 if not ready)
sh install.sh uninstall   # removes the containers and networks, keeps the data directory
sh install.sh uninstall --purge   # also deletes the data directory (asks you to type "delete"; --yes skips that)
```

Add `--data-dir` when the install is not in the default place. Without a local copy, run them as `curl -fsSL https://orglane.app/install.sh | sh -s -- status`.

### Managed services

Managed services (such as Forgejo, from M2) run as Docker Compose projects next to Orglane. They are installed and kept running by the **agent** ([ADR 0019](../docs/adr/0019-agent-transport-and-packages.md), [docs/specs/agent.md](../docs/specs/agent.md)), the only container with access to Docker. They are **off by default**: the installer never enables them on its own, and the interactive installer doesn't ask. The core can't start the agent itself; enable it on the host:

```sh
curl -fsSL https://orglane.app/install.sh | sudo sh -s -- enable-managed   # an existing install
sh install.sh --mode local --managed --yes                                 # a new one, enabled right away
```

`enable-managed` keeps the installed version (unless `--version` is given; the agent must have the core's version) and the secrets, then:

- writes `COMPOSE_PROFILES=managed`, which starts the `agent` service, and `ORGLANE_INSTALL_MANAGED=true`. Later re-runs and upgrades keep managed services enabled;
- **pins the agent image by digest:** it pulls `ghcr.io/orglane/orglane-agent:<version>`, reads the digest the registry gives for that tag (the multi-arch index) and writes `ORGLANE_AGENT_IMAGE=ghcr.io/orglane/orglane-agent:<version>@sha256:<digest>`. If the image can't be pulled or no digest can be resolved, it stops and changes nothing; it never writes a bare tag. `--agent-image` names another image; one that exists only on this machine (built with `docker build --target agent -t orglane-agent:dev .`, no registry digest) is used as it is, like `--image` for the core, with a warning;
- writes `ORGLANE_AGENT_PUBLISH_ADDRESS`, where services publish their own ports (Forgejo SSH): `127.0.0.1` in local and demo mode, every interface (`0.0.0.0`) in domain mode, where those ports must also be open in the firewalls;
- creates `instances/` (mode 700) in the data directory and runs `docker compose up --wait`, which waits until the agent answers its healthcheck.

Every install gets `ORGLANE_AGENT_SECRET` (64 hex characters) in `.env`, the secret the core and the agent share, so enabling managed services later needs no new secret. `status` shows whether the agent is ready (`orglane agent healthcheck` in its container) and lists the service projects. `uninstall` stops the agent first, then runs `docker compose down` for every project under `instances/*/compose.yaml` (project `<COMPOSE_PROJECT_NAME>-<key>`, default `orglane-<key>`), then removes the bundle; their data stays until `--purge`.

**Local and demo mode use `orglane.localhost` with managed services.** Inside a container the bare name `localhost` is the container itself (`/etc/hosts` comes before Docker's DNS), so a service could never reach a workspace at `http://localhost`. With managed services the base domain is `orglane.localhost`: the workspace is `http://orglane.localhost` and services get names such as `http://git.orglane.localhost`. Enabling them on an existing local install therefore **moves the workspace** from `http://localhost` (the installer says so and asks, unless `--yes`); people sign in again at the new address. Chrome, Firefox and Playwright resolve every `*.localhost` name to this machine without configuration. Safari on macOS doesn't resolve subdomains of `localhost`: use another browser, or add `/etc/hosts` entries such as `127.0.0.1 orglane.localhost git.orglane.localhost`. Inside Docker, services reach the workspace on port 80 only, so keep the default `--http-port` when services sign in with Orglane (the installer warns otherwise).

### Domain mode: DNS and reachability first

Let's Encrypt limits failed validations, so the installer requests no certificate until DNS is right. It shows the records to create for this server's public address (asked from `ipv4.icanhazip.com` and `ipv6.icanhazip.com`, or `--public-ip`):

```
    work.acme.com                    A     203.0.113.10
    *.acme.com                       A     203.0.113.10
```

(AAAA records too when the server has a public IPv6 address.) The wildcard is for company services such as `git.acme.com`. It is recommended now and needed once a service is added. Then the installer:

- **refuses** when the workspace host has no A record, resolves to another address, or has an AAAA record that doesn't point at this server (Let's Encrypt prefers IPv6, so a stale AAAA record fails validation). It also refuses when DNS can't be queried at all;
- starts a throwaway Traefik on ports 80 and 443 and fetches it through the workspace host's name. It **refuses** if the ports are taken or the name doesn't lead back to this server;
- only warns about a missing wildcard record, and about `ufw` when it is active and doesn't mention ports 80 and 443.

The reachability check runs from the server itself, so it can't see a hosting provider's firewall: open ports 80 and 443 (TCP) there as well. Behind NAT without hairpin routing, or with split-horizon DNS, the check fails even when everything is right. Then pass `--skip-checks`. On re-runs the DNS check only warns.

### Certificate authority

`--acme-ca` chooses where certificates come from:

- `production`: Let's Encrypt, the default;
- `staging` (or `--acme-staging`): Let's Encrypt staging, whose test certificates browsers don't trust, but without production's rate limits;
- an `https://` ACME directory URL.

The choice is stored in `.env` (`ORGLANE_ACME_CA_SERVER`), and re-runs and upgrades keep it. Only an explicit `--acme-ca` changes it. Traefik keeps the account and certificates of the old CA in `traefik/acme.json` and doesn't replace them when the CA changes. So on a change the installer stops Traefik, deletes `acme.json` (after asking, unless `--yes`) and starts Traefik with the new CA, which issues the certificate again. To move a staging install to production:

```sh
curl -fsSL https://orglane.app/install.sh | sudo sh -s -- --acme-ca production
```

### Advanced settings

These environment variables are kept in `.env` (each must be a single line); a re-run uses the value from its environment if set, else the kept one: `ORGLANE_HTTP_PUBLISH` and `ORGLANE_HTTPS_PUBLISH` (where Traefik is published), `ORGLANE_EDGE_NETWORK_PREFIX` (see [Configuration](#configuration)), `COMPOSE_PROJECT_NAME` (to run several installs on one machine) and `COMPOSE_FILE` (extra Compose files; the installer tests use it for the Pebble overlay). `--http-port` moves local and demo mode to another port, for example `--http-port 8080` for `http://localhost:8080`.

### Troubleshooting

- **A port is in use.** The installer stops and says so. Stop the other program, or move local and demo mode with `--http-port`.
- **The network overlaps.** The core and Traefik share a network with a fixed subnet, `10.231.0.0/24` by default. When another Docker network (for example one left behind by an older install) or a network of the host already uses it, Docker refuses to create it (depending on its version: `Pool overlaps with other one on this address space`, `networks have overlapping IPv4` or `failed to allocate gateway`). The installer then stops and names the subnet. `docker network ls` and `docker network inspect` show the networks Docker has. Remove the old one, or run the installer again with a free subnet in `ORGLANE_EDGE_NETWORK_PREFIX` (the first three octets). The installer keeps it in `.env` for later re-runs:

  ```sh
  curl -fsSL https://orglane.app/install.sh | ORGLANE_EDGE_NETWORK_PREFIX=10.232.0 sh -s -- --mode demo
  # as root (Linux): ... | sudo ORGLANE_EDGE_NETWORK_PREFIX=10.232.0 sh -s -- --mode domain ...
  ```

### Testing the installer

`make test-install` (also part of `make test-integration`) builds the core image from the checkout. It then runs `install.sh` non-interactively from `--source`, in five ways:

- **local mode:** the web UI and `/readyz` through Traefik; `.env` mode 600; the printed setup token creates the owner; a re-run keeps the secrets and the owner; `status`; and `uninstall` with and without `--purge`;
- **demo mode:** one-click sign-in as the demo owner;
- **an edge subnet clash:** with another Docker network on the edge subnet (the same /24, or a partial overlap) the install fails with a message that names `ORGLANE_EDGE_NETWORK_PREFIX`, and a re-run with a free prefix installs and keeps it in `.env`; a malformed prefix (from the environment, or kept in `.env` and edited by hand), and settings or options with more than one line, are refused before anything is written, and `.env` stays as it is;
- **domain mode against Pebble:** a certificate is issued and HTTPS works; a re-run keeps the CA stored in `.env` (`--acme-ca staging`) and `acme.json`; `--acme-ca production` switches the CA, resets `acme.json` and gets a new certificate;
- **domain mode without DNS:** the install is refused before anything is written or started;
- **managed services:** a default install has an agent secret but no agent; `enable-managed` on an existing local install refuses a custom build without `--agent-image` and an agent image it can't pull (`.env` unchanged, no agent), then pins an image pushed to a throwaway local registry by the registry's digest of its tag, moves the workspace to `orglane.localhost`, starts the agent and checks that the core reaches it over the shared socket and has no Docker socket; a re-run keeps managed services (with a locally built agent image used as it is); `uninstall` removes a service project under `instances/`; `--managed` on a fresh install; `enable-managed` on the Pebble domain install keeps its address and certificate and publishes service ports on every interface.

The tests build the agent image too (`docker build --target agent`) and pass it with `--agent-image`, the way `--image` passes the core image: no release is needed. Each test logs `install-time:`, the time from start to a ready workspace. `make lint` runs shellcheck on every shell script (`lint-sh`). Verification on a real VPS with a real domain is part of the M0 exit check.

## Reverse proxy and client addresses

The core takes the client IP (sign-in rate limiting, audit log) from the connection unless the peer is listed in `server.trusted_proxies` (`ORGLANE_SERVER_TRUSTED_PROXIES`); only then is `X-Forwarded-For` honored. Nothing is trusted by default. Any setup with a reverse proxy must give the proxy a fixed address and list exactly that address, and must not publish the core's port directly. The Compose bundle below does this for Traefik (see `ORGLANE_EDGE_NETWORK_PREFIX`); `tests/conformance/compose.yaml` shows a variant that passes the address at start.

## The Compose bundle

```
                    :80 / :443
 internet ──────────► traefik ──── edge network ────► orglane :8080  (public listener: API, web app)
                       │  │                             │
                       │  └── polls ──────────────────► orglane :9090  (internal listener: Traefik only)
                       │                                │      │
                       │                                │      │ unix socket (volume agent-run)
                       │                                │      ▼
                       │                                │    agent (profile managed, network none) ──► /var/run/docker.sock
                       │                                │      │ writes instances/<key>/, runs docker compose
                       │                       database network ▼
                       └── edge network ──────► managed services (projects <project>-<key>)
                                                        │      │
                                                     postgres ◄── services network (no outside access)
```

- **Traefik** (v3, pinned by version and digest) is the only container that publishes ports. It has a fixed address on the edge network (`<ORGLANE_EDGE_NETWORK_PREFIX>.2`), and that address is the only entry in the core's `server.trusted_proxies`, so client IPs come from Traefik's `X-Forwarded-For` and nobody else can set them. It has **no Docker socket** ([ADR 0008](../docs/adr/0008-ingress-domains-tunnels.md)): it polls its routes from the core through Traefik's HTTP provider.
- **The core** serves those routes on an **internal listener** (`server.internal_listen`, `:9090` in the bundle) at `GET /traefik/config`. The internal listener is not published and Traefik does not route to it; the public listener does not serve `/traefik/config`. The routes are generated from the domain settings ([ADR 0018](../docs/adr/0018-domain-model.md)): a router for the workspace host, the security headers middleware and, in domain mode, TLS and the HTTP to HTTPS redirect. See `core/internal/ingress`.
- **PostgreSQL** (18, pinned) sits on an internal network that only the core joins, and on a second internal network, `services`, for managed services' databases (`svc_<key>`). The core is not on `services`.
- **The agent** (profile `managed`, off by default, see [Managed services](#managed-services)) is the only container with the Docker socket. Its image, `ghcr.io/orglane/orglane-agent`, is the same binary as the core plus the Docker CLI and the Compose plugin, pinned by version and SHA-256 (the `agent` target of the root `Dockerfile`, published with the core image). It runs as root (the socket is root-equivalent anyway) with `network_mode: none`, a read-only root filesystem and only `CHOWN`, `DAC_OVERRIDE` and `FOWNER`. The core talks to it over a Unix socket in the `agent-run` volume, which only the two mount, with HMAC-signed requests and answers (`ORGLANE_AGENT_SECRET`); the agent accepts connections only from the core's uid (65532). The core container never gets the Docker socket or a Docker CLI.
- **Managed services** join the edge network, so Traefik routes to them. Traefik carries the workspace host (without a port) as a network alias there (`ORGLANE_EDGE_WORKSPACE_ALIAS`, written by the installer; default the workspace host or base domain), so services reach Orglane by the same name and URL as browsers. Because they share that network with the core, the core's internal listener answers **only Traefik's address** (`server.internal_allowed_peers`, `ORGLANE_SERVER_INTERNAL_ALLOWED_PEERS=<prefix>.2/32`); every other peer gets `403`.
- Every service has a healthcheck and `restart: unless-stopped`. The core image is distroless and runs as a non-root user; the containers run with a read-only root filesystem where possible, no extra capabilities and `no-new-privileges`. Logs rotate at 3 × 10 MB per container.

### Data

Everything persistent lives under `ORGLANE_DATA_DIR` (default `/var/lib/orglane`):

| Path | Contents |
|---|---|
| `postgres/` | PostgreSQL data |
| `traefik/acme.json` | ACME account and certificates (domain mode) |
| `instances/` | Managed services (mode 700): `instances/<key>/` per service with its generated `compose.yaml`, `.env` (secrets, mode 600) and data; `instances/.agent/` holds the agent's state. See [docs/specs/agent.md](../docs/specs/agent.md#instance-directory) |

The files under `instances/<key>/` are generated: you can read them, but the agent overwrites manual edits on its next reconcile (every 10 minutes, and whenever a service's container stops). Settings change through Orglane.

On macOS, set `ORGLANE_DATA_DIR` to a real path under your home directory without symlinks: Docker Desktop does not share `/var/lib`, and it creates missing directories below a symlinked path inside its VM instead of on the Mac.

### Configuration

Settings are environment variables, usually in a `.env` file next to `compose.yaml`: the installer writes it, or copy [`compose/.env.example`](compose/.env.example). `ORGLANE_POSTGRES_PASSWORD` and `ORGLANE_IDENTITY_MASTER_SECRET` are always required. The main switch is `ORGLANE_INGRESS_MODE`. It loads `compose/modes/<mode>.env`, which sets the core's ingress mode, `environment` and `domain.scheme` together:

| Mode | For | What the core tells Traefik |
|---|---|---|
| `local` (default) | Trying Orglane on a laptop (`--demo`), no domain | The workspace host on HTTP (`:80`), no TLS, no HSTS. The core runs with `environment: development` and `domain.scheme: http`, since plain HTTP is a development-only setting |
| `domain` | Production on a real domain | The workspace host on HTTPS (`:443`) with a Let's Encrypt certificate, HSTS, HTTP redirected to HTTPS. The core runs with `environment: production` and `domain.scheme: https` |

Other variables (all optional; see `.env.example`):

| Variable | Default | Meaning |
|---|---|---|
| `ORGLANE_DOMAIN_BASE` | `localhost` | Company base domain (`domain.base`) |
| `ORGLANE_DOMAIN_WORKSPACE_HOST` | the base domain | Workspace host (`domain.workspace_host`) |
| `ORGLANE_DEMO_ENABLED` | `false` | Demo mode: loads the "Acme Robotics" dataset into an empty database and allows one-click sign-in. Local mode only |
| `ORGLANE_ACME_EMAIL` | empty | Contact for the Let's Encrypt account (domain mode) |
| `ORGLANE_ACME_CA_SERVER` | Let's Encrypt production | ACME directory, e.g. Let's Encrypt staging |
| `ORGLANE_HTTP_PUBLISH`, `ORGLANE_HTTPS_PUBLISH` | `127.0.0.1:80`, `127.0.0.1:443` | Where Traefik is published. Set `80` and `443` to listen on every interface in domain mode |
| `ORGLANE_DATA_DIR` | `/var/lib/orglane` | Host path for persistent data |
| `ORGLANE_IMAGE` | `ghcr.io/orglane/orglane:latest` | Core image; `make image` builds `orglane:dev` |
| `ORGLANE_POSTGRES_*` | see below | PostgreSQL sizing |
| `ORGLANE_EDGE_NETWORK_PREFIX` | `10.231.0` | First three octets of the Traefik/core network (`<prefix>.0/24`); change it if it collides with a host network |
| `ORGLANE_LOG_LEVEL`, `ORGLANE_TRAEFIK_LOG_LEVEL` | `info`, `INFO` | Log levels |
| `COMPOSE_PROFILES` | empty | `managed` starts the agent (managed services) |
| `ORGLANE_AGENT_SECRET` | empty | Secret shared by the core and the agent, at least 32 characters (`openssl rand -hex 32`). Empty disables managed services |
| `ORGLANE_AGENT_IMAGE` | `ghcr.io/orglane/orglane-agent:<release>` | Agent image; pin it by digest (`...:<version>@sha256:<digest>`), same version as the core. `docker build --target agent -t orglane-agent:dev .` builds one |
| `ORGLANE_AGENT_PUBLISH_ADDRESS` | `127.0.0.1` | Host address for ports that managed services publish (Forgejo SSH); `0.0.0.0` in domain mode |
| `ORGLANE_EDGE_WORKSPACE_ALIAS` | the workspace host, else the base domain | Traefik's alias on the edge network for managed services: the workspace host without a port |

In local mode with managed services, set `ORGLANE_DOMAIN_BASE=orglane.localhost` (see [Managed services](#managed-services)).

The core validates the combination on start. Domain mode refuses `localhost` and a workspace host with a port, because no public certificate can be issued for them, and it requires `production` and `https`. Local mode requires `http`.

Security headers sent on every response: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin` and a restrictive `Permissions-Policy`; in domain mode also `Strict-Transport-Security: max-age=31536000` (`ingress.hsts_max_age`, without `includeSubDomains`, so a workspace on the base domain does not force HTTPS on the company's other subdomains).

### PostgreSQL sizing

The defaults are light, for a small host ([ADR 0009](../docs/adr/0009-resource-philosophy.md)). The installer scales them with the memory Docker reports (`docker info`) using these rules. In local and demo mode it counts at most 2 GB, so a trial doesn't claim a laptop's memory; below 1 GB it keeps the defaults. Without the installer, set them by hand:

| Variable | Default | Rule of thumb |
|---|---|---|
| `ORGLANE_POSTGRES_SHARED_BUFFERS` | `128MB` | 15–25 % of RAM (installer: 25 %, at most 8 GB) |
| `ORGLANE_POSTGRES_EFFECTIVE_CACHE_SIZE` | `512MB` | 50 % of RAM |
| `ORGLANE_POSTGRES_WORK_MEM` | `4MB` | 4–16 MB (installer: 4 MB below 3.5 GB, 8 MB below 16 GB, else 16 MB) |
| `ORGLANE_POSTGRES_MAINTENANCE_WORK_MEM` | `64MB` | 5 % of RAM, at least 64 MB, at most 1 GB |
| `ORGLANE_POSTGRES_MAX_CONNECTIONS` | `50` | Raise when managed services share the instance: each service's role may use up to 20 (installer: 100 from 3.5 GB) |

For example, a 4 GB VPS: `1GB`, `2GB`, `8MB`, `256MB`, `100`.

### Running it

The installer does all of this. By hand: releases publish the image (`ghcr.io/orglane/orglane`, see [docs/releasing.md](../docs/releasing.md)); to run the code of a checkout, build it locally (or use `sh deploy/install.sh --source . --image orglane:dev`):

```sh
make image                                    # builds orglane:dev
cd deploy/compose
cp .env.example .env                          # then set ORGLANE_POSTGRES_PASSWORD, ORGLANE_IDENTITY_MASTER_SECRET, ORGLANE_DATA_DIR
echo ORGLANE_IMAGE=orglane:dev >> .env
docker compose up -d --wait
curl http://localhost/readyz                  # local mode
docker compose logs orglane | grep setup_token   # one-time token to create the owner at http://localhost
```

For a domain, set the domain-mode variables in `.env`, point DNS for the workspace host at the server, open ports 80 and 443, and run `docker compose up -d --wait`. Traefik requests the certificate on its first poll of the core (HTTP-01 challenge on port 80).

#### From Let's Encrypt staging to production

The installer does this with `--acme-ca` (see [Certificate authority](#certificate-authority)). By hand: to avoid production rate limits on a first try, set `ORGLANE_ACME_CA_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory` in `.env`. Browsers don't trust staging certificates. Once a staging certificate is issued, switch to production as follows. `docker compose restart` is not enough: it keeps the container's old environment and therefore the staging CA. Also, Traefik keeps the staging account and certificate in `traefik/acme.json` and doesn't replace them when the CA changes.

```sh
cd deploy/compose
docker compose stop traefik        # Traefik rewrites acme.json from memory while it runs
# In .env, delete the ORGLANE_ACME_CA_SERVER line (production is the default).
sudo rm "$ORGLANE_DATA_DIR/traefik/acme.json"  # the staging account and certificate, owned by root
docker compose up -d --wait --force-recreate traefik   # new container with the production CA
```

Use the same path as `ORGLANE_DATA_DIR` in `.env` (default `/var/lib/orglane`). Within a minute, `curl -sv https://<workspace host>/readyz` should show a certificate issued by Let's Encrypt (`issuer: C=US; O=Let's Encrypt`) and no staging issuer such as `(STAGING)`. The same steps apply when changing to any other ACME CA.

`docker compose down` stops the bundle and keeps the data; delete `ORGLANE_DATA_DIR` to start over.

### Testing domain mode with Pebble

[Pebble](https://github.com/letsencrypt/pebble) is Let's Encrypt's ACME test server. The overlay `compose/pebble/compose.pebble.yaml` adds it to the bundle, points Traefik's resolver at it, switches the core to domain mode for `work.orglane.test`, and gives Traefik that name as a network alias, so Pebble validates the HTTP-01 challenge against Traefik inside the Compose network. No public DNS or open ports are involved.

The automated test does all of this (`make test-compose`, also part of `make test-integration`). The same target starts the bundle with `COMPOSE_PROFILES=managed` and the agent image built from the checkout: the agent becomes healthy and reaches Docker, the core reaches the agent, the core container has neither the Docker socket nor a Docker CLI, PostgreSQL is on the services network and the core is not, and a container on the edge network reaches the workspace by its name (`orglane.localhost`) but gets `403` from the core's internal listener. By hand:

```sh
make image
cd deploy/compose
export ORGLANE_IMAGE=orglane:dev ORGLANE_POSTGRES_PASSWORD=$(openssl rand -hex 16) \
  ORGLANE_IDENTITY_MASTER_SECRET=$(openssl rand -base64 32) \
  ORGLANE_DATA_DIR=$(mktemp -d /tmp/orglane-pebble.XXXX) \
  ORGLANE_HTTP_PUBLISH=127.0.0.1:8080 ORGLANE_HTTPS_PUBLISH=127.0.0.1:8443
docker compose -f compose.yaml -f pebble/compose.pebble.yaml up -d --wait

# The root that signs issued certificates (Pebble's API uses its static test CA).
curl -s --cacert pebble/pebble.minica.pem "https://$(docker compose -f compose.yaml -f pebble/compose.pebble.yaml port pebble 15000)/roots/0" > /tmp/pebble-root.pem
# A few seconds after start, the workspace is served with a Pebble certificate:
curl -i --cacert /tmp/pebble-root.pem --resolve work.orglane.test:8443:127.0.0.1 https://work.orglane.test:8443/readyz
curl -i -H 'Host: work.orglane.test' http://127.0.0.1:8080/readyz   # 301 to https://work.orglane.test/readyz
docker compose -f compose.yaml -f pebble/compose.pebble.yaml logs pebble | grep 'Issued certificate'

docker compose -f compose.yaml -f pebble/compose.pebble.yaml down
```

Never use the Pebble overlay in production: Pebble keeps everything in memory and its certificates are trusted by nobody.
