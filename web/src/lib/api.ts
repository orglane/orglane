import {
  createOrglaneClient,
  isProblem,
  type OrglaneClient,
  type Problem,
  ProblemError,
  type Schema,
  toProblem,
} from "@orglane/api-client";
import { createOrglaneQueryHooks, type OrglaneQueryHooks } from "@orglane/api-client/react";
import { queryOptions } from "@tanstack/react-query";

/**
 * The web app's connection to the Orglane API: the same public API every
 * other client uses (ADR 0001), authenticated by the browser session cookie.
 *
 * State-changing requests carry the session's CSRF token (from sign-in or
 * `GET /api/v1/me`) in `X-CSRF-Token`; every request carries the UI locale
 * in `Accept-Language`.
 */
export interface Api {
  client: OrglaneClient;
  hooks: OrglaneQueryHooks;
  setCsrfToken(token: string | undefined): void;
  setLocale(locale: string): void;
}

export interface ApiOptions {
  /** Defaults to "" (the origin the app is served from). */
  baseUrl?: string;
  fetch?: (request: Request) => Promise<Response>;
}

const safeMethods = new Set(["GET", "HEAD", "OPTIONS"]);

export function createApi(options: ApiOptions = {}): Api {
  let csrfToken: string | undefined;
  let locale = "en";
  const client = createOrglaneClient({
    baseUrl: options.baseUrl ?? "",
    fetch: options.fetch,
  });
  client.use({
    onRequest({ request }) {
      request.headers.set("Accept-Language", locale);
      if (csrfToken && !safeMethods.has(request.method)) {
        request.headers.set("X-CSRF-Token", csrfToken);
      }
      return request;
    },
  });
  return {
    client,
    hooks: createOrglaneQueryHooks(client),
    setCsrfToken(token) {
      csrfToken = token;
    },
    setLocale(next) {
      locale = next;
    },
  };
}

export type Me = Schema<"Me">;
export type SignInResult = Schema<"SignInResult">;

/** An API failure: problem details plus what the UI needs to explain it. */
export class ApiError extends Error {
  override readonly name = "ApiError";
  readonly problem: Problem;
  /** Seconds to wait before retrying (429), from `Retry-After`. */
  readonly retryAfter: number | undefined;

  constructor(problem: Problem, retryAfter?: number) {
    super(problem.detail ?? problem.title);
    this.problem = problem;
    this.retryAfter = retryAfter;
  }

  get status(): number {
    return this.problem.status;
  }
}

/** A request that never reached the server (offline, DNS, CORS, ...). */
export class NetworkError extends Error {
  override readonly name = "NetworkError";
}

interface Result<Data> {
  data?: Data;
  error?: unknown;
  response: Response;
}

/**
 * Awaits an API call and returns its data, or throws an {@link ApiError}
 * (non-2xx) or a {@link NetworkError}.
 */
export async function call<Data>(request: Promise<Result<Data>>): Promise<Data> {
  let result: Result<Data>;
  try {
    result = await request;
  } catch (err) {
    if (err instanceof ProblemError) {
      throw new ApiError(err.problem);
    }
    throw new NetworkError(err instanceof Error ? err.message : String(err), { cause: err });
  }
  const { data, error, response } = result;
  if (!response.ok) {
    const retry = Number.parseInt(response.headers.get("Retry-After") ?? "", 10);
    throw new ApiError(toProblem(error, response), Number.isFinite(retry) ? retry : undefined);
  }
  return data as Data;
}

/** Reports whether `err` is an API error with one of the given statuses. */
export function hasStatus(err: unknown, ...statuses: number[]): err is ApiError {
  return err instanceof ApiError && statuses.includes(err.status);
}

export const meKey = ["me"] as const;

/**
 * The signed-in person, or null when signed out. Keeps the CSRF token of the
 * session current.
 */
export function meQuery(api: Api) {
  return queryOptions({
    queryKey: meKey,
    queryFn: async (): Promise<Me | null> => {
      try {
        const me = await call(api.client.GET("/api/v1/me"));
        api.setCsrfToken(me.csrfToken);
        return me;
      } catch (err) {
        if (hasStatus(err, 401)) {
          api.setCsrfToken(undefined);
          return null;
        }
        throw err;
      }
    },
    staleTime: 60_000,
  });
}

export const setupStatusKey = ["auth", "setup"] as const;

export function setupStatusQuery(api: Api) {
  return queryOptions({
    queryKey: setupStatusKey,
    queryFn: () => call(api.client.GET("/api/v1/auth/setup")),
    staleTime: 30_000,
  });
}

export type { Schema };
export { isProblem };
