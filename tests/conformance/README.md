# OpenID Conformance Suite

`make conformance` runs the [OpenID Conformance Suite](https://gitlab.com/openid/conformance-suite) (release pinned in `run.sh` and `compose.yaml`) against a locally started Orglane, for the profile Orglane implements (ADR 0003):

| Plan | Variant |
|---|---|
| `oidcc-config-certification-test-plan` | discovery document and JWKS |
| `oidcc-basic-certification-test-plan` | `server_metadata=discovery`, `client_registration=static_client` (authorization code flow, `client_secret_basic`) |

Everything runs in Docker (`compose.yaml`), so it needs Docker, Go, `git` and `openssl`, and takes a few minutes once the images are pulled:

1. Builds the web UI and `orglane` for Linux from this checkout and starts it with PostgreSQL, behind a Caddy TLS proxy as `https://op.orglane.test` (a throwaway CA, trusted by the suite only).
2. Starts the suite's published images (server, nginx, MongoDB).
3. Creates the owner with the setup token and registers two OIDC clients with `orglane cli oidc-client add`.
4. Writes the suite configuration (`make_config.py`): static clients, and browser automation that fills in Orglane's sign-in screen at `/sign-in`. The suite's HtmlUnit browser runs no JavaScript modules, so it gets the web UI's basic sign-in form (see [web/README.md](../../web/README.md#sign-in-for-applications-oidc)).
5. Runs the plans with the suite's own `scripts/run-test-plan.py` and exports the results to `.conformance/results`: one zip per plan with the JSON logs (signed, from the runner) and, in `html/`, one with the HTML report (`export_html.py`, also after a failed run).

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

## CI

The `Conformance` workflow (`.github/workflows/conformance.yml`) runs `make conformance` on `ubuntu-latest` and uploads `.conformance/results` (the suite's JSON and HTML export per plan) as the `conformance-results-*` artifact. It is not part of pull request CI. Start it for any branch with `gh workflow run conformance.yml --ref <branch>`; it also runs weekly on main. The job fails on the same conditions as a local run.

## Concurrent runs

Each run uses its own Compose project, `orglane-conformance-<checksum of the work directory>`, with its own containers, volumes and network. Docker picks the subnet, and Orglane trusts `X-Forwarded-For` only from the proxy address Docker assigned in that run. Nothing publishes host ports. So runs from different worktrees (or with different `CONFORMANCE_WORK` directories) can run at the same time. Start-up and cleanup only touch that run's project. A run removes its project on exit, also after a failure or Ctrl-C, and the next run in the same work directory removes anything a killed run left behind. A second run in a work directory that is in use is refused: the work directory holds a `.lock` while a run is active.

`KEEP=1 make conformance` leaves the containers running and prints the `docker compose -p orglane-conformance-<n> down -v` command that stops them. PKCE, refresh token rotation, passkeys and the other parts of #6 that the plans above do not cover are tested by the Go integration tests in `core/internal/identity/web`.
