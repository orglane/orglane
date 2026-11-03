import { describe, expect, it } from "vitest";

import { createOrglaneClient } from "./client.js";
import { isProblem, type Problem, ProblemError, toProblem, unwrap } from "./problem.js";

const forbidden: Problem = {
  type: "about:blank",
  title: "Forbidden",
  status: 403,
  detail: "missing permission instance.read",
  instance: "/api/v1/instance",
};

function clientAnswering(response: () => Response) {
  return createOrglaneClient({ baseUrl: "https://work.acme.test", fetch: async () => response() });
}

describe("isProblem", () => {
  it("accepts problem details", () => {
    expect(isProblem(forbidden)).toBe(true);
    expect(isProblem({ type: "about:blank", title: "Not Found", status: 404 })).toBe(true);
  });

  it("rejects anything else", () => {
    expect(isProblem(undefined)).toBe(false);
    expect(isProblem(null)).toBe(false);
    expect(isProblem("Forbidden")).toBe(false);
    expect(isProblem({ status: "unavailable" })).toBe(false);
    expect(isProblem({ type: "about:blank", title: "Forbidden", status: "403" })).toBe(false);
    expect(isProblem({ ...forbidden, detail: 42 })).toBe(false);
  });
});

describe("toProblem", () => {
  const response = new Response(null, { status: 502, statusText: "Bad Gateway" });

  it("returns problem bodies unchanged", () => {
    expect(toProblem(forbidden, response)).toBe(forbidden);
  });

  it("builds an about:blank problem from the status otherwise", () => {
    expect(toProblem(undefined, response)).toEqual({
      type: "about:blank",
      title: "Bad Gateway",
      status: 502,
    });
    expect(toProblem({ status: "unavailable" }, response)).toEqual({
      type: "about:blank",
      title: "Bad Gateway",
      status: 502,
    });
  });

  it("keeps a text body as the detail", () => {
    expect(toProblem("upstream timed out", new Response(null, { status: 504 }))).toEqual({
      type: "about:blank",
      title: "HTTP 504",
      status: 504,
      detail: "upstream timed out",
    });
  });
});

describe("unwrap", () => {
  it("returns the data of a successful request", async () => {
    const client = clientAnswering(() => Response.json({ version: "v0.1.0" }));
    await expect(unwrap(client.GET("/api/v1/version"))).resolves.toEqual({ version: "v0.1.0" });
  });

  it("throws a ProblemError parsed from a problem+json response", async () => {
    const client = clientAnswering(
      () =>
        new Response(JSON.stringify(forbidden), {
          status: 403,
          headers: { "content-type": "application/problem+json" },
        }),
    );

    const error = await unwrap(client.GET("/api/v1/instance")).catch((e: unknown) => e);

    expect(error).toBeInstanceOf(ProblemError);
    const problemError = error as ProblemError;
    expect(problemError.problem).toEqual(forbidden);
    expect(problemError.status).toBe(403);
    expect(problemError.message).toBe("missing permission instance.read");
    expect(problemError.response?.status).toBe(403);
  });

  it("throws a ProblemError for error responses without a problem body", async () => {
    const client = clientAnswering(
      () => new Response(null, { status: 503, statusText: "Service Unavailable" }),
    );

    const error = await unwrap(client.GET("/readyz")).catch((e: unknown) => e);

    expect(error).toBeInstanceOf(ProblemError);
    expect((error as ProblemError).problem).toEqual({
      type: "about:blank",
      title: "Service Unavailable",
      status: 503,
    });
  });
});
