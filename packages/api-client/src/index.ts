/**
 * Typed client for the Orglane API, generated from the OpenAPI contract
 * (core/api/openapi.yaml). React Query hooks live in `@orglane/api-client/react`.
 */

export {
  createOrglaneClient,
  type OrglaneClient,
  type OrglaneClientOptions,
} from "./client.js";
export type { components, operations, paths } from "./generated/schema.js";
export {
  type FetchResult,
  isProblem,
  PROBLEM_CONTENT_TYPE,
  type Problem,
  ProblemError,
  toProblem,
  unwrap,
} from "./problem.js";

import type { components } from "./generated/schema.js";

/** A schema of the contract by name, such as `Schema<"Version">`. */
export type Schema<Name extends keyof components["schemas"]> = components["schemas"][Name];
