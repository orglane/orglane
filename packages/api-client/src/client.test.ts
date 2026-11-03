import { describe, expect, expectTypeOf, it } from "vitest";

import { createOrglaneClient, type OrglaneClient, type Schema } from "./index.js";

/** A fetch double that records requests and answers with a fixed response. */
function mockFetch(respond: (request: Request) => Response) {
  const requests: Request[] = [];
  const fetch = async (request: Request) => {
    requests.push(request);
    return respond(request);
  };
  return { fetch, requests };
}

function json(body: unknown, status = 200, contentType = "application/json"): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": contentType } });
}

describe("createOrglaneClient", () => {
  it("calls the health probe against the base URL", async () => {
    const { fetch, requests } = mockFetch(() => json({ status: "ok" }));
    const client = createOrglaneClient({ baseUrl: "https://work.acme.test/", fetch });

    const { data, error, response } = await client.GET("/healthz");

    expect(error).toBeUndefined();
    expect(response.status).toBe(200);
    expect(data).toEqual({ status: "ok" });
    expect(requests).toHaveLength(1);
    expect(requests[0]?.method).toBe("GET");
    expect(requests[0]?.url).toBe("https://work.acme.test/healthz");
  });

  it("calls versioned endpoints under /api/v1", async () => {
    const { fetch, requests } = mockFetch(() => json({ version: "v0.1.0" }));
    const client = createOrglaneClient({ baseUrl: "https://work.acme.test", fetch });

    const { data } = await client.GET("/api/v1/version");

    expect(data?.version).toBe("v0.1.0");
    expect(requests[0]?.url).toBe("https://work.acme.test/api/v1/version");
  });

  it("sends the configured headers, overridable per request", async () => {
    const { fetch, requests } = mockFetch(() =>
      json({ version: "v0.1.0", baseDomain: "acme.test", workspaceHost: "work.acme.test" }),
    );
    const client = createOrglaneClient({
      baseUrl: "https://work.acme.test",
      fetch,
      headers: { "x-orglane-test": "client", "accept-language": "en" },
    });

    await client.GET("/api/v1/instance", { headers: { "x-orglane-test": "request" } });

    expect(requests[0]?.headers.get("x-orglane-test")).toBe("request");
    expect(requests[0]?.headers.get("accept-language")).toBe("en");
  });

  it("returns problem+json error bodies as typed errors", async () => {
    const problem = {
      type: "about:blank",
      title: "Forbidden",
      status: 403,
      detail: "missing permission instance.read",
      instance: "/api/v1/instance",
    };
    const { fetch } = mockFetch(() => json(problem, 403, "application/problem+json"));
    const client = createOrglaneClient({ baseUrl: "https://work.acme.test", fetch });

    const { data, error, response } = await client.GET("/api/v1/instance");

    expect(data).toBeUndefined();
    expect(response.status).toBe(403);
    expect(error).toEqual(problem);
  });

  it("uses globalThis.fetch when no fetch is given", () => {
    expect(() => createOrglaneClient({ baseUrl: "https://work.acme.test" })).not.toThrow();
  });
});

// Type-level tests: checked by `tsc --noEmit` (pnpm run typecheck, part of `make lint`).
// The `it` wrappers only keep the assertions inside the suite; they do no runtime work.
describe("types", () => {
  const client: OrglaneClient = createOrglaneClient({ baseUrl: "https://work.acme.test" });
  // Never called: only the types of these functions matter.
  const healthz = () => client.GET("/healthz");
  const version = () => client.GET("/api/v1/version");
  const instance = () => client.GET("/api/v1/instance");

  it("types response bodies from the contract", () => {
    type HealthzData = NonNullable<Awaited<ReturnType<typeof healthz>>["data"]>;
    expectTypeOf<HealthzData>().toEqualTypeOf<Schema<"Health">>();
    expectTypeOf<HealthzData["status"]>().toEqualTypeOf<"ok" | "unavailable">();

    type VersionData = NonNullable<Awaited<ReturnType<typeof version>>["data"]>;
    expectTypeOf<VersionData>().toEqualTypeOf<{ version: string }>();

    type InstanceData = NonNullable<Awaited<ReturnType<typeof instance>>["data"]>;
    expectTypeOf<InstanceData>().toEqualTypeOf<Schema<"Instance">>();
  });

  it("types error bodies as problem details", () => {
    type InstanceError = NonNullable<Awaited<ReturnType<typeof instance>>["error"]>;
    expectTypeOf<InstanceError>().toEqualTypeOf<Schema<"Problem">>();
  });

  it("rejects paths and methods that are not in the contract", () => {
    // Wrapped in functions that never run: these lines only need to fail type checking.
    const unknownPath = () =>
      // @ts-expect-error: "/api/v1/nope" is not a path of the contract.
      client.GET("/api/v1/nope");
    const unknownMethod = () =>
      // @ts-expect-error: /healthz has no POST operation.
      client.POST("/healthz");
    expect(unknownPath).toBeTypeOf("function");
    expect(unknownMethod).toBeTypeOf("function");
  });
});
