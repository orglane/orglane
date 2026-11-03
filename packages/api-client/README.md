# @orglane/api-client

Typed client for the Orglane API, generated from the OpenAPI contract in [`core/api/openapi.yaml`](../../core/api/openapi.yaml). The web and mobile apps use it to talk to the same public API as everyone else. Licensed under Apache-2.0 (see [`../LICENSE`](../LICENSE)).

- Types: [openapi-typescript](https://openapi-ts.dev/) generates `src/generated/schema.ts` (committed).
- Requests: [openapi-fetch](https://openapi-ts.dev/openapi-fetch/), a thin typed wrapper around `fetch`.
- React: [openapi-react-query](https://openapi-ts.dev/openapi-react-query/) hooks on TanStack Query v5 (an optional peer dependency), exported from `@orglane/api-client/react`.

## Usage

```ts
import { createOrglaneClient, isProblem, unwrap } from "@orglane/api-client";

const client = createOrglaneClient({
  baseUrl: "https://work.acme.com", // or "" for the origin the app is served from
  headers: { "accept-language": "en" }, // optional, sent with every request
  // fetch: customFetch,               // optional, defaults to globalThis.fetch
});

const { data, error } = await client.GET("/api/v1/version");
if (error) {
  // Error bodies are RFC 9457 problem details (application/problem+json).
  console.log(error.status, error.title, error.detail);
}

// Or throw a ProblemError on any non-2xx response:
const instance = await unwrap(client.GET("/api/v1/instance"));
```

```tsx
import { createOrglaneQueryHooks } from "@orglane/api-client/react";

const api = createOrglaneQueryHooks(client);

function Version() {
  const { data } = api.useQuery("get", "/api/v1/version");
  return <span>{data?.version}</span>;
}
```

Failed queries reject with the parsed error body; use `isProblem()` to narrow it. The client has no authentication logic: pass credentials through `headers`, a custom `fetch`, or `client.use(middleware)`.

## Regenerating

Change the contract first, then run `make generate` from the repository root; it regenerates the Go server code and `src/generated/schema.ts`. `make lint` (and this package's tests) fail when the generated file is stale.

The generator lives in [`codegen/`](codegen), a private workspace package with its own TypeScript 5: openapi-typescript drives the TypeScript compiler API, which the workspace's TypeScript 7 does not ship.
