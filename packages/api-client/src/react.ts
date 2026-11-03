import createQueryClient, { type OpenapiQueryClient } from "openapi-react-query";

import type { OrglaneClient } from "./client.js";
import type { paths } from "./generated/schema.js";

/** TanStack Query hooks typed against the Orglane API contract. */
export type OrglaneQueryHooks = OpenapiQueryClient<paths>;

/**
 * Creates TanStack Query (v5) hooks bound to an Orglane client.
 *
 * ```tsx
 * const api = createOrglaneQueryHooks(createOrglaneClient({ baseUrl: "" }));
 * const { data } = api.useQuery("get", "/api/v1/version");
 * ```
 *
 * A failed request rejects with the response body, which for the Orglane API
 * is a {@link Problem}; check it with `isProblem`.
 */
export function createOrglaneQueryHooks(client: OrglaneClient): OrglaneQueryHooks {
  return createQueryClient(client);
}

export type { Problem } from "./problem.js";
