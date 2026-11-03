import type { components } from "./generated/schema.js";

/** RFC 9457 problem details, the body of every error response of the Orglane API. */
export type Problem = components["schemas"]["Problem"];

/** The media type of problem details responses. */
export const PROBLEM_CONTENT_TYPE = "application/problem+json";

/** Reports whether `value` is a problem details object as defined by the contract. */
export function isProblem(value: unknown): value is Problem {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const v = value as Record<string, unknown>;
  return (
    typeof v.type === "string" &&
    typeof v.title === "string" &&
    typeof v.status === "number" &&
    (v.detail === undefined || typeof v.detail === "string") &&
    (v.instance === undefined || typeof v.instance === "string")
  );
}

/**
 * Turns the `error` of a failed request into problem details.
 *
 * A body that already is a problem is returned as is. Anything else (an empty
 * body, plain text from a proxy, JSON of another shape) becomes an
 * `about:blank` problem built from the response status, as RFC 9457 suggests;
 * a text body is kept as `detail`.
 */
export function toProblem(error: unknown, response: Response): Problem {
  if (isProblem(error)) {
    return error;
  }
  const problem: Problem = {
    type: "about:blank",
    title: response.statusText || `HTTP ${response.status}`,
    status: response.status,
  };
  if (typeof error === "string" && error.trim() !== "") {
    problem.detail = error;
  }
  return problem;
}

/** An error carrying the problem details of a failed request. */
export class ProblemError extends Error {
  override readonly name = "ProblemError";
  readonly problem: Problem;
  readonly response: Response | undefined;

  constructor(problem: Problem, options: { response?: Response; cause?: unknown } = {}) {
    super(problem.detail ?? problem.title, { cause: options.cause });
    this.problem = problem;
    this.response = options.response;
  }

  /** The HTTP status code of the problem. */
  get status(): number {
    return this.problem.status;
  }
}

/** The result shape of an `openapi-fetch` request. */
export type FetchResult<Data, Err> =
  | { data: Data; error?: never; response: Response }
  | { data?: never; error: Err; response: Response };

/**
 * Returns the data of a successful request, or throws a {@link ProblemError}
 * for a non-2xx response.
 *
 * ```ts
 * const version = await unwrap(client.GET("/api/v1/version"));
 * ```
 */
export async function unwrap<Data, Err>(
  result: FetchResult<Data, Err> | Promise<FetchResult<Data, Err>>,
): Promise<Data> {
  const { data, error, response } = await result;
  if (!response.ok) {
    throw new ProblemError(toProblem(error, response), { response });
  }
  return data as Data;
}
