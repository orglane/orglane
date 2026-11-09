import { createMemoryHistory } from "@tanstack/react-router";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { App } from "@/app";
import { createApi } from "@/lib/api";

/** A recorded request to the fake API. */
export interface Call {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Headers;
  body: unknown;
}

export type Reply = { status: number; body?: unknown; headers?: Record<string, string> };
type Handler = Reply | ((call: Call) => Reply | Promise<Reply>);
type Route = (call: Call) => Reply | Promise<Reply> | undefined;

export const me = {
  person: {
    id: "0190a1b2-0000-7000-8000-000000000001",
    username: "alex",
    workEmail: "alex@acme.com",
    displayName: "Alex Morgan",
  },
  permissions: ["instance.read"],
  passkeyOffer: false,
  csrfToken: "csrf-123",
  session: {
    id: "0190a1b2-0000-7000-8000-0000000000aa",
    device: {
      id: "0190a1b2-0000-7000-8000-0000000000dd",
      name: "Chrome on macOS",
      platform: "web",
    },
    createdAt: "2026-11-01T10:00:00Z",
    lastSeenAt: "2026-11-03T09:59:00Z",
    expiresAt: "2026-12-01T10:00:00Z",
    authMethods: ["password"],
    current: true,
  },
};

export const signInResult = {
  person: me.person,
  csrfToken: "csrf-123",
  passkeyOffer: false,
  sessionExpiresAt: "2026-12-01T10:00:00Z",
};

const problem = (status: number, title: string): Reply => ({
  status,
  body: { type: "about:blank", title, status },
  headers: { "content-type": "application/problem+json" },
});

export const replies = {
  unauthorized: problem(401, "Unauthorized"),
  forbidden: problem(403, "Forbidden"),
  conflict: problem(409, "Conflict"),
  tooMany: (seconds: number): Reply => ({
    ...problem(429, "Too Many Requests"),
    headers: { "content-type": "application/problem+json", "retry-after": String(seconds) },
  }),
};

/**
 * A fake Orglane API: `routes` answer by "METHOD /path"; a signed-out,
 * already set up instance by default.
 */
export function fakeApi(routes: Record<string, Handler> = {}) {
  const calls: Call[] = [];
  const defaults: Record<string, Reply> = {
    "GET /api/v1/auth/setup": { status: 200, body: { required: false } },
    "GET /api/v1/me": replies.unauthorized,
  };
  const route: Route = (call) => {
    const key = `${call.method} ${call.path}`;
    const r = routes[key] ?? defaults[key];
    return typeof r === "function" ? r(call) : r;
  };
  const fetch = async (request: Request): Promise<Response> => {
    const url = new URL(request.url);
    const text = await request.text();
    const call: Call = {
      method: request.method,
      path: url.pathname,
      query: url.searchParams,
      headers: request.headers,
      body: text ? JSON.parse(text) : undefined,
    };
    calls.push(call);
    const reply = (await route(call)) ?? {
      status: 404,
      body: { type: "about:blank", title: "Not Found", status: 404 },
    };
    const body = reply.body === undefined ? null : JSON.stringify(reply.body);
    return new Response(reply.status === 204 ? null : body, {
      status: reply.status,
      headers: { "content-type": "application/json", ...reply.headers },
    });
  };
  const api = createApi({ baseUrl: "http://localhost:3000", fetch });
  return {
    api,
    calls,
    called: (method: string, path: string) =>
      calls.filter((c) => c.method === method && c.path === path),
  };
}

/** Renders the whole app at `path` against a fake API. */
export function renderApp(path: string, routes: Parameters<typeof fakeApi>[0] = {}) {
  const fake = fakeApi(routes);
  const history = createMemoryHistory({ initialEntries: [path] });
  const user = userEvent.setup();
  const view = render(<App api={fake.api} history={history} />);
  return { ...fake, history, user, ...view };
}
