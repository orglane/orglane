# core

The Orglane core: a single Go binary (`cmd/orglane`) with `server`, `agent` and `cli` modes. It belongs to the root Go module `github.com/orglane/orglane`.

```sh
orglane server [-config orglane.yaml]       # API, OpenID Provider, job queue; applies migrations and seeds built-in roles on start
orglane cli migrate [-config orglane.yaml]  # apply migrations, seed built-in roles and exit
orglane cli setup-token                     # new one-time token for creating the first owner (while none exists)
orglane cli oidc-client add -id ... -name ... -redirect-uri ... [-public] [-native] [-refresh-tokens]
orglane cli version
orglane agent                               # stub: logs "not implemented" and exits 0
```

## Layout

| Path | Contents |
|---|---|
| `api/openapi.yaml` | The API contract (OpenAPI 3). Source of truth for routes, models and permissions |
| `api/oapi-codegen.yaml` | Code generation settings (`make generate`) |
| `migrations/` | goose SQL migrations, embedded into the binary. UUIDv7 keys, `timestamptz` columns |
| `internal/api` | Implements the generated server, enforces `x-orglane-permission`, problem+json errors |
| `internal/api/apigen` | Generated code. Do not edit; `make lint` fails when it is stale |
| `internal/audit` | Append-only audit log writer; entries are written in the transaction of the change |
| `internal/authz` | Permission registry, built-in Owner / Admin / Member bundles, principals, the authenticator seam |
| `internal/config` | YAML + environment configuration, including the ADR 0018 domain model |
| `internal/crypto` | Keys derived from the instance master secret (HKDF), AES-256-GCM sealing, argon2id password hashing, random tokens |
| `internal/database` | pgx pool (UTC sessions), migrations (goose for the core schema, River for its queue tables) |
| `internal/identity` | Domain layer for people, roles, devices, sessions, passwords, passkeys, first-run setup and OIDC clients; every change is audited |
| `internal/identity/oidcop` | The embedded OpenID Provider (zitadel/oidc): storage over our tables, signing keys and rotation |
| `internal/identity/web` | Browser side: session and device cookies, CSRF, client IP, rate limiting, the API authenticator, the OIDC login page |
| `internal/jobs` | In-process Postgres-backed job queue (River) |
| `internal/logging` | `log/slog` setup: JSON in production, text in development |
| `internal/server` | Server mode wiring and graceful shutdown |
| `internal/store` | sqlc query layer generated from `store/queries/*.sql` (`make generate`). Do not edit `*.gen.go` |

## HTTP surface

- `GET /healthz`: liveness. `GET /readyz`: the database is reachable and all migrations are applied (503 otherwise).
- `GET /.well-known/orglane`: instance discovery (workspace URL, API base URL, OIDC issuer, version).
- Everything else in the contract lives under `/api/v1`. Errors are RFC 9457 problem details (`application/problem+json`).
- Every operation declares the permission it requires with `x-orglane-permission` (ADR 0004): a registry permission, `public`, or `authenticated` (self-service on the caller's own account).
- The OpenID Provider (ADR 0003) serves `/.well-known/openid-configuration` and `/oidc/*` (authorize, token, userinfo, introspect, revoke, end_session, keys, login). Its issuer is the workspace URL.

## Identity (#6)

- **First-run setup:** while no owner exists, `POST /api/v1/auth/setup` creates the first owner, but only with the one-time setup token printed to the log on first start (`orglane cli setup-token` makes a new one). Only its hash is stored.
- **Sign-in:** password (argon2id, OWASP parameters stored with each hash) or passkey (WebAuthn, relying party ID = the workspace host). After sign-in the API reports `passkeyOffer`; skipping it is remembered for 30 days.
- **Sessions:** an opaque token in a host-only cookie (`__Host-orglane_session`; `orglane_session` in the plain-HTTP demo mode), HttpOnly, SameSite=Lax, bound to a `web` device record (`__Host-orglane_device`). Only the token hash is stored; every request is checked against the database, so revocation is immediate. Idle and absolute expiry (`identity.session_*`).
- **CSRF:** state-changing requests authenticated with the cookie need the workspace `Origin` and the session's `X-CSRF-Token`; sign-in from another origin is refused.
- **Brute force:** per client IP (in memory) and per account (in the database, also for unknown emails), with audit entries. The client IP is the connection's peer; `X-Forwarded-For` is honored only from the proxies listed in `server.trusted_proxies` (none by default; deployments list their Traefik address).
- **API authentication:** the session cookie or an OIDC access token (`Authorization: Bearer`). Access tokens are opaque and checked against the database on every request. Permissions come from the person's roles, resolved per request.
- **OIDC:** authorization code flow (PKCE with S256 required for public clients, `plain` refused), refresh tokens rotated on every use with reuse detection, single-use codes (reuse revokes the grant), RS256 ID tokens. Clients are registered with the CLI.
- **Signing keys:** RSA 2048, generated by the server, stored encrypted with a key derived from `identity.master_secret` (required in production). A new key replaces the active one every `identity.signing_key_rotation` (90 days); old keys stay in the JWKS for `identity.signing_key_grace` (7 days).
- **Conformance:** `make conformance` runs the OpenID Conformance Suite (`oidcc-config` and `oidcc-basic` plans) against a local server in Docker; see `tests/conformance/README.md`.

## Resource defaults (ADR 0009)

- The connection pool keeps no idle connections without load (`MinConns: 0`, `database.max_conns` caps it).
- The job queue picks up jobs through LISTEN/NOTIFY, with a 5s fallback poll (`jobs.fetch_poll_interval`).
- The example periodic job (`jobs.noop`) is disabled by default.
- `make measure-memory` reports idle RSS (about 20 MiB on an arm64 Mac at the time of writing).

## Data model (M0)

| Table | Contents |
|---|---|
| `people` | Directory base: username (unique), work email (unique case-insensitively), display name, status `invited \| active \| suspended \| offboarded` |
| `roles`, `role_permissions`, `person_roles` | Roles as permission bundles (ADR 0004); built-in roles are seeded from `internal/authz` |
| `devices` | A person's browsers and apps (`web`, `ios`, `android`); revocable (ADR 0012) |
| `sessions` | Bound to a device of the same person (composite foreign key). Only the SHA-256 of the token is stored |
| `audit_entries` | Append-only (triggers reject `UPDATE`, `DELETE`, `TRUNCATE`): actor, action, target, metadata, IP, user agent |
| `person_credentials`, `passkeys` | Password hash (argon2id PHC string), passkey offer dismissal, WebAuthn credentials |
| `webauthn_ceremonies`, `sign_in_throttles` | Short-lived passkey challenges (single use); failed password sign-ins and lockouts per account |
| `oidc_clients`, `oidc_auth_requests`, `oidc_access_tokens`, `oidc_refresh_tokens` | OIDC clients (secret hashes), authorization requests and codes (hashes), tokens grouped by grant |
| `signing_keys` | Token signing keys; private keys sealed with a key derived from the master secret |
| `instance_settings` | Runtime instance settings as JSON values. The domain model (ADR 0018) stays in the config file and environment |

Teams, memberships, profile fields, account links and capabilities arrive as new tables referencing `people.id`.

