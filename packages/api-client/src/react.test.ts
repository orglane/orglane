import { QueryClient } from "@tanstack/react-query";
import { describe, expect, expectTypeOf, it } from "vitest";

import { createOrglaneClient, isProblem, type Schema } from "./index.js";
import { createOrglaneQueryHooks } from "./react.js";

function hooksAnswering(response: () => Response) {
  const client = createOrglaneClient({
    baseUrl: "https://work.acme.test",
    fetch: async () => response(),
  });
  return createOrglaneQueryHooks(client);
}

// The hooks themselves need a React renderer; queryOptions() is what they pass to
// TanStack Query, so exercising it with a QueryClient covers the wiring.
describe("createOrglaneQueryHooks", () => {
  it("exposes the TanStack Query hooks", () => {
    const api = hooksAnswering(() => Response.json({}));
    expect(api.useQuery).toBeTypeOf("function");
    expect(api.useSuspenseQuery).toBeTypeOf("function");
    expect(api.useMutation).toBeTypeOf("function");
  });

  it("fetches typed data through queryOptions", async () => {
    const api = hooksAnswering(() => Response.json({ version: "v0.1.0" }));
    const options = api.queryOptions("get", "/api/v1/version");

    expect(options.queryKey).toEqual(["get", "/api/v1/version"]);
    const data = await new QueryClient().fetchQuery(options);
    expect(data).toEqual({ version: "v0.1.0" });
    expectTypeOf(data).toEqualTypeOf<Schema<"Version">>();
  });

  it("rejects with the problem details of a failed request", async () => {
    const problem = { type: "about:blank", title: "Unauthorized", status: 401 };
    const api = hooksAnswering(
      () =>
        new Response(JSON.stringify(problem), {
          status: 401,
          headers: { "content-type": "application/problem+json" },
        }),
    );

    const error = await new QueryClient({ defaultOptions: { queries: { retry: false } } })
      .fetchQuery(api.queryOptions("get", "/api/v1/instance"))
      .catch((e: unknown) => e);

    expect(isProblem(error)).toBe(true);
    expect(error).toEqual(problem);
  });
});
