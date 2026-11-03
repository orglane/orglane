# deploy

Docker Compose files and, later, the one-command install script (#11).

| Path | Contents |
|---|---|
| `compose/` | The production bundle: Traefik, the Orglane core and PostgreSQL |
| `compose/pebble/` | **Testing only:** an overlay that runs domain mode against Pebble, Let's Encrypt's ACME test server |
| `dev/` | Local development only: a PostgreSQL for `make dev` and the matching `orglane.yaml` |

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

Settings are environment variables, usually in `deploy/compose/.env` (copy [`compose/.env.example`](compose/.env.example)). `ORGLANE_POSTGRES_PASSWORD` and `ORGLANE_IDENTITY_MASTER_SECRET` are always required. The main switch is `ORGLANE_INGRESS_MODE`. It loads `compose/modes/<mode>.env`, which sets the core's ingress mode, `environment` and `domain.scheme` together:

| Mode | For | What the core tells Traefik |
|---|---|---|
| `local` (default) | Trying Orglane on a laptop (`--demo`), no domain | The workspace host on HTTP (`:80`), no TLS, no HSTS. The core runs with `environment: development` and `domain.scheme: http`, since plain HTTP is a development-only setting |
| `domain` | Production on a real domain | The workspace host on HTTPS (`:443`) with a Let's Encrypt certificate, HSTS, HTTP redirected to HTTPS. The core runs with `environment: production` and `domain.scheme: https` |

Other variables (all optional; see `.env.example`):

| Variable | Default | Meaning |
|---|---|---|
| `ORGLANE_DOMAIN_BASE` | `localhost` | Company base domain (`domain.base`) |
| `ORGLANE_DOMAIN_WORKSPACE_HOST` | the base domain | Workspace host (`domain.workspace_host`) |
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

The defaults are light, for a small host ([ADR 0009](../docs/adr/0009-resource-philosophy.md)). The installer (#11) will scale them with the host's memory using these rules; set them by hand until then:

| Variable | Default | Rule of thumb |
|---|---|---|
| `ORGLANE_POSTGRES_SHARED_BUFFERS` | `128MB` | 15–25 % of RAM |
| `ORGLANE_POSTGRES_EFFECTIVE_CACHE_SIZE` | `512MB` | 50 % of RAM |
| `ORGLANE_POSTGRES_WORK_MEM` | `4MB` | 4–16 MB |
| `ORGLANE_POSTGRES_MAINTENANCE_WORK_MEM` | `64MB` | 5 % of RAM, at most 1 GB |
| `ORGLANE_POSTGRES_MAX_CONNECTIONS` | `50` | Raise when managed services share the instance |

For example, a 4 GB VPS: `1GB`, `2GB`, `8MB`, `256MB`, `100`.

### Running it

Releases publish the image (`ghcr.io/orglane/orglane`, see [docs/releasing.md](../docs/releasing.md)). To run the code of a checkout, build it locally:

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

To avoid production rate limits on a first try, set `ORGLANE_ACME_CA_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory` in `.env`. Browsers don't trust staging certificates. Once a staging certificate is issued, switch to production as follows. `docker compose restart` is not enough: it keeps the container's old environment and therefore the staging CA. Also, Traefik keeps the staging account and certificate in `traefik/acme.json` and doesn't replace them when the CA changes.

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
