/**
 * Generated from core/api/openapi.yaml by openapi-typescript.
 * Do not edit: change the contract and run `make generate`.
 */

export interface paths {
    "/healthz": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Liveness probe
         * @description Returns 200 while the process is running. Does not check dependencies.
         */
        get: operations["getHealthz"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/readyz": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Readiness probe
         * @description Returns 200 when the instance can serve traffic: the database is
         *     reachable and all migrations are applied. Returns 503 otherwise.
         *     Check details are logged by the server, not returned.
         */
        get: operations["getReadyz"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/version": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Server version */
        get: operations["getVersion"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instance": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Instance information
         * @description Returns how this instance is addressed (ADR 0018).
         */
        get: operations["getInstance"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        /** @description RFC 9457 problem details. */
        Problem: {
            /**
             * @description A URI reference identifying the problem type. `about:blank` means the HTTP status says it all.
             * @example about:blank
             */
            type: string;
            /**
             * @description A short, human-readable summary of the problem type.
             * @example Forbidden
             */
            title: string;
            /**
             * @description The HTTP status code.
             * @example 403
             */
            status: number;
            /** @description A human-readable explanation specific to this occurrence. */
            detail?: string;
            /** @description A URI reference identifying this occurrence (the request path). */
            instance?: string;
        };
        Health: {
            /** @enum {string} */
            status: "ok" | "unavailable";
            checks?: components["schemas"]["HealthCheck"][];
        };
        HealthCheck: {
            /** @example database */
            name: string;
            /** @enum {string} */
            status: "ok" | "unavailable";
        };
        Version: {
            /** @example v0.1.0 */
            version: string;
        };
        Instance: {
            /** @example v0.1.0 */
            version: string;
            /**
             * @description The company base domain.
             * @example acme.com
             */
            baseDomain: string;
            /**
             * @description The host the workspace is served on.
             * @example work.acme.com
             */
            workspaceHost: string;
        };
    };
    responses: {
        /** @description The request has no valid credentials. */
        Unauthorized: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description The caller lacks the permission this operation requires. */
        Forbidden: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description An unexpected server error. */
        InternalError: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
    };
    parameters: never;
    requestBodies: never;
    headers: never;
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    getHealthz: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The process is alive. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Health"];
                };
            };
        };
    };
    getReadyz: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The instance is ready. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Health"];
                };
            };
            /** @description The instance is not ready. */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Health"];
                };
            };
        };
    };
    getVersion: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The server version. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Version"];
                };
            };
            500: components["responses"]["InternalError"];
        };
    };
    getInstance: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Instance information. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Instance"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
}
