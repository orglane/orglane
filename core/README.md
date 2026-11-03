# core

The Orglane core: a single Go binary (`cmd/orglane`) with `server`, `agent` and `cli` modes. It belongs to the root Go module `github.com/orglane/orglane`.

```sh
orglane server [-config orglane.yaml]       # API, job queue; applies migrations and seeds built-in roles on start
orglane cli migrate [-config orglane.yaml]  # apply migrations, seed built-in roles and exit
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
| `internal/authz` | Permission registry, built-in Owner / Admin / Member bundles, principals, the authenticator seam (identity lands in #6) |
| `internal/config` | YAML + environment configuration, including the ADR 0018 domain model |
| `internal/database` | pgx pool (UTC sessions), migrations (goose for the core schema, River for its queue tables) |
| `internal/identity` | Domain layer for people, roles, devices and sessions; every change is audited |
| `internal/jobs` | In-process Postgres-backed job queue (River) |
| `internal/logging` | `log/slog` setup: JSON in production, text in development |
| `internal/server` | Server mode wiring and graceful shutdown |
| `internal/store` | sqlc query layer generated from `store/queries/*.sql` (`make generate`). Do not edit `*.gen.go` |

## HTTP surface

- `GET /healthz`: liveness. `GET /readyz`: the database is reachable and all migrations are applied (503 otherwise).
- Everything else lives under `/api/v1`. Errors are RFC 9457 problem details (`application/problem+json`).
- Every operation declares the permission it requires with `x-orglane-permission` (ADR 0004). Non-public operations return 401 until the identity provider lands, because the default authenticator denies everything.

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
| `instance_settings` | Runtime instance settings as JSON values. The domain model (ADR 0018) stays in the config file and environment |

Teams, memberships, profile fields, account links and capabilities arrive as new tables referencing `people.id`.

