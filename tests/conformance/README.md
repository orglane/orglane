# OpenID Conformance Suite

`make conformance` runs the [OpenID Conformance Suite](https://gitlab.com/openid/conformance-suite) (release pinned in `run.sh` and `compose.yaml`) against a locally started Orglane, for the profile Orglane implements (ADR 0003):

| Plan | Variant |
|---|---|
| `oidcc-config-certification-test-plan` | discovery document and JWKS |
| `oidcc-basic-certification-test-plan` | `server_metadata=discovery`, `client_registration=static_client` (authorization code flow, `client_secret_basic`) |

Everything runs in Docker (`compose.yaml`), so it needs Docker, Go, `git` and `openssl`, and takes a few minutes once the images are pulled:

1. Builds `orglane` for Linux from this checkout and starts it with PostgreSQL, behind a Caddy TLS proxy as `https://op.orglane.test` (a throwaway CA, trusted by the suite only).
2. Starts the suite's published images (server, nginx, MongoDB).
3. Creates the owner with the setup token and registers two OIDC clients with `orglane cli oidc-client add`.
4. Writes the suite configuration (`make_config.py`): static clients, and browser automation that fills in Orglane's sign-in form at `/oidc/login`.
5. Runs the plans with the suite's own `scripts/run-test-plan.py` and exports the results to `.conformance/results`.

The run fails on any failure, warning or skip that is not listed, with its reason, in `expected-failures.json` or `expected-skips.json`:

| Module | Result | Why |
|---|---|---|
| `oidcc-server` | warning | zitadel/oidc adds a `client_id` claim to ID tokens |
| `oidcc-scope-profile` | warning | the directory has no given/family name, picture, locale, ... yet (M1 profile fields) |
| `oidcc-claims-essential` | warning | the optional `claims` request parameter is not supported |
| `oidcc-ensure-request-with-acr-values-succeeds` | warning | no `acr` values are defined |
| `oidcc-scope-address`, `-phone`, `-all` | skipped | the address and phone scopes are not offered |
| `oidcc-unsigned-request-object-...` | skipped | request objects are not supported |

`oidcc-prompt-login`, `oidcc-max-age-1`, `oidcc-ensure-registered-redirect-uri` and `oidcc-ensure-request-object-with-redirect-uri` finish in "review": the suite keeps a screenshot of the login or error page as evidence for a human reviewer (needed only for a formal certification).

Last result (suite release-v5.3.4): 36 modules, 1722 conditions passed, 0 failures, the 5 expected warnings above. `oidcc-server-rotate-keys` is not part of these plans (it has to be started by hand after rotating keys); rotation is covered by `TestSigningKeyRotation` in `core/internal/identity/web`.

`KEEP=1 make conformance` leaves the containers running; stop them with `docker compose -p orglane-conformance down -v`. PKCE, refresh token rotation, passkeys and the other parts of #6 that the plans above do not cover are tested by the Go integration tests in `core/internal/identity/web`.
