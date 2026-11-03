# deploy

Docker Compose files and the one-command install script.

| Path | Contents |
|---|---|
| `dev/` | Local development only: a PostgreSQL for `make dev` and the matching `orglane.yaml` |

The production Compose bundle and install script arrive with #9 and #11.

## Reverse proxy and client addresses

The core takes the client IP (sign-in rate limiting, audit log) from the connection unless the peer is listed in `server.trusted_proxies` (`ORGLANE_SERVER_TRUSTED_PROXIES`); only then is `X-Forwarded-For` honored. Nothing is trusted by default. A Compose bundle with Traefik must give Traefik a fixed address on a fixed subnet and list exactly that address, e.g. `ORGLANE_SERVER_TRUSTED_PROXIES=172.30.0.2/32`, and must not publish the core's port directly. `tests/conformance/compose.yaml` shows the pattern.
