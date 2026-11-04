# deploy

The one-command installer and the Docker Compose files.

| Path | Contents |
|---|---|
| `install.sh` | The installer: checks the machine, writes the bundle and its settings, starts Orglane and prints the URL |
| `compose/` | The production bundle: Traefik, the Orglane core and PostgreSQL |
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

These environment variables are kept in `.env`; a re-run uses the value from its environment if set, else the kept one: `ORGLANE_HTTP_PUBLISH` and `ORGLANE_HTTPS_PUBLISH` (where Traefik is published), `ORGLANE_EDGE_NETWORK_PREFIX` (see [Configuration](#configuration)), `COMPOSE_PROJECT_NAME` (to run several installs on one machine) and `COMPOSE_FILE` (extra Compose files; the installer tests use it for the Pebble overlay). `--http-port` moves local and demo mode to another port, for example `--http-port 8080` for `http://localhost:8080`.

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
- **an edge subnet clash:** with another Docker network on the edge subnet (the same /24, or a partial overlap) the install fails with a message that names `ORGLANE_EDGE_NETWORK_PREFIX`, and a re-run with a free prefix installs and keeps it in `.env`; a malformed prefix is refused before anything is written;
- **domain mode against Pebble:** a certificate is issued and HTTPS works; a re-run keeps the CA stored in `.env` (`--acme-ca staging`) and `acme.json`; `--acme-ca production` switches the CA, resets `acme.json` and gets a new certificate;
- **domain mode without DNS:** the install is refused before anything is written or started.

Each test logs `install-time:`, the time from start to a ready workspace. `make lint` runs shellcheck on every shell script (`lint-sh`). Verification on a real VPS with a real domain is part of the M0 exit check.

## Reverse proxy and client addresses

The core takes the client IP (sign-in rate limiting, audit log) from the connection unless the peer is listed in `server.trusted_proxies` (`ORGLANE_SERVER_TRUSTED_PROXIES`); only then is `X-Forwarded-For` honored. Nothing is trusted by default. Any setup with a reverse proxy must give the proxy a fixed address and list exactly that address, and must not publish the core's port directly. The Compose bundle below does this for Traefik (see `ORGLANE_EDGE_NETWORK_PREFIX`); `tests/conformance/compose.yaml` shows a variant that passes the address at start.

## The Compose bundle

```
                    :80 / :443
 internet ──────────► traefik ──── edge network ────► orglane :8080  (public listener: API, web app)
                         │                              │
                         └── polls ───────────────────► orglane :9090  (internal listener: Traefik routes)
                                                        │
                                       database network (no outside access)
                                                        │
                                                     postgres
```

- **Traefik** (v3, pinned by version and digest) is the only container that publishes ports. It has a fixed address on the edge network (`<ORGLANE_EDGE_NETWORK_PREFIX>.2`), and that address is the only entry in the core's `server.trusted_proxies`, so client IPs come from Traefik's `X-Forwarded-For` and nobody else can set them. It has **no Docker socket** ([ADR 0008](../docs/adr/0008-ingress-domains-tunnels.md)): it polls its routes from the core through Traefik's HTTP provider.
- **The core** serves those routes on an **internal listener** (`server.internal_listen`, `:9090` in the bundle) at `GET /traefik/config`. The internal listener is not published and Traefik does not route to it; the public listener does not serve `/traefik/config`. The routes are generated from the domain settings ([ADR 0018](../docs/adr/0018-domain-model.md)): a router for the workspace host, the security headers middleware and, in domain mode, TLS and the HTTP to HTTPS redirect. See `core/internal/ingress`.
- **PostgreSQL** (18, pinned) sits on an internal network that only the core joins.
- Every service has a healthcheck and `restart: unless-stopped`. The core image is distroless and runs as a non-root user; the containers run with a read-only root filesystem where possible, no extra capabilities and `no-new-privileges`. Logs rotate at 3 × 10 MB per container.

### Data

Everything persistent lives under `ORGLANE_DATA_DIR` (default `/var/lib/orglane`):

| Path | Contents |
|---|---|
| `postgres/` | PostgreSQL data |
| `traefik/acme.json` | ACME account and certificates (domain mode) |

Managed services will add `instances/<id>/` ([ADR 0006](../docs/adr/0006-agent-and-desired-state.md)). On macOS, set `ORGLANE_DATA_DIR` to a real path under your home directory without symlinks: Docker Desktop does not share `/var/lib`, and it creates missing directories below a symlinked path inside its VM instead of on the Mac.

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
| `ORGLANE_POSTGRES_MAX_CONNECTIONS` | `50` | Raise when managed services share the instance (installer: 100 from 3.5 GB) |

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

The automated test does all of this (`make test-compose`, also part of `make test-integration`). By hand:

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
