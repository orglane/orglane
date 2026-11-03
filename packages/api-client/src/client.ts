import createClient, { type Client, type HeadersOptions } from "openapi-fetch";

import type { paths } from "./generated/schema.js";

/** A typed client for the Orglane API. Every method is checked against the OpenAPI contract. */
export type OrglaneClient = Client<paths>;

/** Options for {@link createOrglaneClient}. */
export interface OrglaneClientOptions {
  /**
   * Origin the API is served from, such as `https://work.acme.com`. Paths in the
   * contract are absolute (`/healthz`, `/api/v1/...`), so the base URL must not
   * include `/api/v1`. A browser app served by the core can pass `""` to call
   * its own origin.
   */
  baseUrl: string;
  /** Replaces `globalThis.fetch`, for example in tests or to add credentials. */
  fetch?: ((request: Request) => Promise<Response>) | undefined;
  /** Headers sent with every request. Per-request headers take precedence. */
  headers?: HeadersOptions | undefined;
}

/**
 * Creates a typed client for the Orglane API.
 *
 * The client does not authenticate on its own: pass credentials through
 * `headers` or a custom `fetch`, or register a middleware with `client.use()`.
 */
export function createOrglaneClient(options: OrglaneClientOptions): OrglaneClient {
  const { baseUrl, fetch, headers } = options;
  return createClient<paths>({
    baseUrl,
    ...(fetch !== undefined && { fetch }),
    ...(headers !== undefined && { headers }),
  });
}
