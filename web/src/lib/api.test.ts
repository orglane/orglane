import { describe, expect, it } from "vitest";

import { fakeApi, replies } from "@/test/harness";

import { ApiError, call, NetworkError } from "./api";

describe("the API client", () => {
  it("sends the CSRF token on state-changing requests only, and the locale on all", async () => {
    const { api, calls } = fakeApi({
      "GET /api/v1/me/sessions": { status: 200, body: { items: [] } },
      "POST /api/v1/me/passkey-offer/dismiss": { status: 204 },
    });
    api.setCsrfToken("token-1");
    api.setLocale("de");
    await call(api.client.GET("/api/v1/me/sessions"));
    await call(api.client.POST("/api/v1/me/passkey-offer/dismiss"));
    expect(calls[0]?.headers.get("X-CSRF-Token")).toBeNull();
    expect(calls[1]?.headers.get("X-CSRF-Token")).toBe("token-1");
    expect(calls.map((c) => c.headers.get("Accept-Language"))).toEqual(["de", "de"]);
  });

  it("turns error responses into ApiError with Retry-After", async () => {
    const { api } = fakeApi({ "POST /api/v1/auth/sign-in": replies.tooMany(120) });
    const err = await call(
      api.client.POST("/api/v1/auth/sign-in", { body: { email: "a@b.c", password: "x" } }),
    ).catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(429);
    expect(err.retryAfter).toBe(120);
  });

  it("reports unreachable servers as NetworkError", async () => {
    const { createApi } = await import("./api");
    const api = createApi({
      baseUrl: "http://localhost:3000",
      fetch: () => Promise.reject(new TypeError("Failed to fetch")),
    });
    await expect(call(api.client.GET("/api/v1/version"))).rejects.toBeInstanceOf(NetworkError);
  });
});
