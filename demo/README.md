# demo

Demo datasets: human-readable, versioned fixture files used by demo mode, screenshots, end-to-end tests and module previews. They are embedded into the `orglane` binary (package `github.com/orglane/orglane/demo`).

## The "Acme Robotics" dataset (`acme/`)

| File | Contents |
|---|---|
| `acme/dataset.yaml` | Version, company name and email domain, the default person for one-click sign-in (the Owner) |
| `acme/people.yaml` | 14 people: username, display name, work email, title, role (1 Owner, 2 Admins, the rest Members) and avatar |
| `acme/avatars/*.svg` | Initials avatars, generated with `go generate ./demo` (a test fails when they are stale) |

Work emails use `acme-robotics.example`, a domain reserved for documentation (RFC 2606). Every person is active and has no password. Teams (M1) and profile fields will be added as new files (`teams.yaml`, ...) and keys; bump `version` when the dataset changes in a way loaders or tests must know about.

## Demo mode

```sh
make demo         # local Postgres (Docker) + `orglane server --demo` on http://localhost:8080, own database orglane_demo
make demo-reset   # drop the demo database; the next `make demo` loads the dataset afresh
```

Or run the binary directly against an **empty** database:

```sh
orglane server --demo -config orglane.yaml   # or demo.enabled: true / ORGLANE_DEMO_ENABLED=true
```

On an empty database the server loads the dataset through the identity service (every person, role and avatar gets its audit entry), skips the first-run setup token and logs a `DEMO MODE` banner. Restarts keep the data. The server **refuses to start** in demo mode on a database that already has people that were not loaded as a demo.

In demo mode:

- `GET /.well-known/orglane` has `"demo": true`; clients show a clear demo marker.
- `GET /api/v1/demo` lists the people one can sign in as.
- `POST /api/v1/demo/sign-in` with `{"username": "..."}` (or no body, for the Owner) creates a normal session and sets the session cookie: one-click sign-in.

Outside demo mode these two operations do not exist (404). Anyone who can reach a demo instance can sign in as any demo person, including the Owner, so never put real data into one.

## Using the dataset in tests and scripts

- **Go end-to-end tests** (against a running `orglane server --demo`): package `github.com/orglane/orglane/tests/demotest`. `demotest.Dataset(t)` returns the dataset, `demotest.SignIn(t, baseURL, "")` signs in as the Owner (or any username) and returns a session for API requests. See `tests/integration/demo_test.go`.
- **In-process Go tests** in `core/`: `demomode.Prepare(ctx, pool, identityService, dataset)` loads the dataset into a test database. See `core/internal/demomode/demomode_integration_test.go`.
- **Screenshot and other scripts:** start `make demo` (or `orglane server --demo` on an empty database), then sign in with one request and reuse the cookie:

  ```sh
  curl -c cookies.txt -X POST http://localhost:8080/api/v1/demo/sign-in \
    -H 'Content-Type: application/json' -d '{"username": "maya.okafor"}'
  curl -b cookies.txt http://localhost:8080/api/v1/people
  ```

  A browser automation (Playwright) does the same `POST` from the page, or clicks the demo button on the sign-in screen once the web app has it.
