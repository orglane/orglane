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
    "/.well-known/orglane": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Instance discovery
         * @description How clients (the mobile app, ADR 0012) find this instance's
         *     workspace, API and OpenID Provider from a work email domain
         *     (ADR 0018). Contains nothing sensitive.
         */
        get: operations["getOrglaneDiscovery"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/setup": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * First-run setup status
         * @description Reports whether the instance still needs its first owner.
         */
        get: operations["getSetupStatus"];
        put?: never;
        /**
         * Create the first owner
         * @description Creates the first owner with a password and signs them in. Requires
         *     the one-time setup token printed in the server log on first start
         *     (or generated with `orglane cli setup-token`). Possible only while no
         *     owner exists. Sets the session cookie.
         */
        post: operations["completeSetup"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/sign-in": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Sign in with a password
         * @description Signs in with a work email and password and sets the session cookie.
         *     Failures are rate limited per client address and per account; a
         *     locked account answers 429 even for the right password.
         */
        post: operations["signInWithPassword"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/passkey/options": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Start a passkey sign-in
         * @description Returns WebAuthn request options for `navigator.credentials.get`
         *     (discoverable credentials, no username needed) and the ceremony ID to
         *     finish with. The ceremony expires after 5 minutes and can be used
         *     once.
         */
        post: operations["beginPasskeySignIn"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/passkey": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Sign in with a passkey
         * @description Verifies the WebAuthn assertion and sets the session cookie.
         */
        post: operations["signInWithPasskey"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/sign-out": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Sign out
         * @description Revokes the current browser session and clears the session cookie.
         */
        post: operations["signOut"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * The signed-in person
         * @description Returns the caller, their permissions and, for browser sessions, the
         *     session, its CSRF token and whether to offer adding a passkey.
         */
        get: operations["getMe"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/sessions": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List my sessions
         * @description The caller's valid sessions, oldest first.
         */
        get: operations["listMySessions"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/sessions/{sessionId}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Revoke one of my sessions
         * @description Revokes the session immediately; its next request is unauthenticated.
         */
        delete: operations["revokeMySession"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/passkeys": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** List my passkeys */
        get: operations["listMyPasskeys"];
        put?: never;
        /**
         * Add a passkey
         * @description Verifies the WebAuthn attestation from `navigator.credentials.create` and stores the passkey.
         */
        post: operations["addMyPasskey"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/passkeys/options": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Start adding a passkey
         * @description Returns WebAuthn creation options for `navigator.credentials.create`
         *     and the ceremony ID to finish with (`POST /api/v1/me/passkeys`).
         */
        post: operations["beginPasskeyRegistration"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/passkeys/{passkeyId}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /** Remove a passkey */
        delete: operations["removeMyPasskey"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/me/passkey-offer/dismiss": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Skip the passkey offer
         * @description Remembers that the caller skipped adding a passkey; the offer returns after 30 days.
         */
        post: operations["dismissPasskeyOffer"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/people": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List people
         * @description The people directory: everyone except offboarded people, sorted by
         *     display name. Any signed-in person may read it. Not paginated yet.
         */
        get: operations["listPeople"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/people/{personId}/avatar": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * A person's avatar image
         * @description Returns the avatar image with its stored content type. Responses
         *     carry `X-Content-Type-Options: nosniff` and a sandboxing
         *     `Content-Security-Policy`, so an image opened directly cannot run
         *     scripts or load anything.
         */
        get: operations["getPersonAvatar"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/demo": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Demo mode information
         * @description Describes the demo dataset and the people one can sign in as with one
         *     click. Served only in demo mode (`orglane server --demo`); otherwise
         *     the path does not exist (404). The sign-in screen shows it when the
         *     discovery document has `demo: true`.
         */
        get: operations["getDemo"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/demo/sign-in": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * One-click sign-in as a demo person
         * @description Signs in as a person of the demo dataset without a credential and
         *     sets the session cookie, like a password sign-in. Without a body (or
         *     without `username`) it signs in as the dataset's default person, the
         *     Owner. Served only in demo mode; otherwise the path does not exist
         *     (404). Attempts are rate limited per client address.
         */
        post: operations["signInAsDemoPerson"];
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
        OrglaneDiscovery: {
            /**
             * @description The workspace URL.
             * @example https://work.acme.com
             */
            workspaceUrl: string;
            /**
             * @description The base URL of this API.
             * @example https://work.acme.com/api/v1
             */
            apiBaseUrl: string;
            /**
             * @description The OpenID Connect issuer (append `/.well-known/openid-configuration` for its metadata).
             * @example https://work.acme.com
             */
            issuer: string;
            /** @example v0.1.0 */
            version: string;
            /**
             * @description True when the instance runs in demo mode: its data is the demo
             *     dataset and anyone who can reach it may sign in as a demo person
             *     with one click (`GET /api/v1/demo`, `POST /api/v1/demo/sign-in`).
             *     Clients should show a clear "demo" marker.
             */
            demo: boolean;
        };
        SetupStatus: {
            /** @description True while no owner exists and setup can be completed with the setup token. */
            required: boolean;
        };
        SetupRequest: {
            /** @description The one-time setup token from the server log or `orglane cli setup-token`. */
            setupToken: string;
            /** @example alex@acme.com */
            workEmail: string;
            /** @example alex */
            username: string;
            /** @example Alex Morgan */
            displayName: string;
            /**
             * Format: password
             * @description At least 12 characters.
             */
            password: string;
        };
        PasswordSignInRequest: {
            /** @example alex@acme.com */
            email: string;
            /** Format: password */
            password: string;
        };
        PasskeyCeremony: {
            /**
             * Format: uuid
             * @description Identifies the ceremony when finishing it.
             */
            ceremonyId: string;
            /**
             * @description WebAuthn options (PublicKeyCredentialCreationOptions or
             *     PublicKeyCredentialRequestOptions) with binary values in
             *     base64url, as accepted by `PublicKeyCredential.parse*OptionsFromJSON`.
             */
            publicKey: {
                [key: string]: unknown;
            };
        };
        PasskeySignInRequest: {
            /** Format: uuid */
            ceremonyId: string;
            /** @description The PublicKeyCredential from `navigator.credentials.get`, serialized with `toJSON()`. */
            credential: {
                [key: string]: unknown;
            };
        };
        AddPasskeyRequest: {
            /** Format: uuid */
            ceremonyId: string;
            /** @description A name for the passkey, e.g. "MacBook". Defaults to "Passkey". */
            name?: string;
            /** @description The PublicKeyCredential from `navigator.credentials.create`, serialized with `toJSON()`. */
            credential: {
                [key: string]: unknown;
            };
        };
        PersonSummary: {
            /** Format: uuid */
            id: string;
            username: string;
            workEmail: string;
            displayName: string;
        };
        SignInResult: {
            person: components["schemas"]["PersonSummary"];
            /** @description Send it in the `X-CSRF-Token` header of state-changing requests. */
            csrfToken: string;
            /** @description True when the person has no passkey and has not skipped the offer recently; offer to add one, with a "Skip" option. */
            passkeyOffer: boolean;
            /** Format: date-time */
            sessionExpiresAt: string;
        };
        Me: {
            person: components["schemas"]["PersonSummary"];
            /** @description The permissions the caller holds (ADR 0004). */
            permissions: string[];
            passkeyOffer: boolean;
            session?: components["schemas"]["Session"];
            /** @description Present for browser sessions. */
            csrfToken?: string;
        };
        Device: {
            /** Format: uuid */
            id: string;
            /** @example Firefox on macOS */
            name: string;
            /** @enum {string} */
            platform: "web" | "ios" | "android";
        };
        Session: {
            /** Format: uuid */
            id: string;
            device: components["schemas"]["Device"];
            /**
             * Format: date-time
             * @description When the person signed in.
             */
            createdAt: string;
            /** Format: date-time */
            lastSeenAt: string;
            /**
             * Format: date-time
             * @description The absolute expiry; the session also ends after a period of inactivity.
             */
            expiresAt: string;
            /** @description `demo` is the one-click sign-in of demo mode (no credential). */
            authMethods: ("password" | "passkey" | "demo")[];
            /** @description True for the session making the request. */
            current: boolean;
        };
        SessionList: {
            items: components["schemas"]["Session"][];
        };
        Passkey: {
            /** Format: uuid */
            id: string;
            name: string;
            /** Format: date-time */
            createdAt: string;
            /** Format: date-time */
            lastUsedAt?: string;
        };
        PasskeyList: {
            items: components["schemas"]["Passkey"][];
        };
        Person: {
            /** Format: uuid */
            id: string;
            /** @example maya.okafor */
            username: string;
            /** @example maya.okafor@acme.com */
            workEmail: string;
            /** @example Maya Okafor */
            displayName: string;
            /**
             * @description The job title, if set.
             * @example Chief Executive Officer
             */
            title?: string;
            /** @enum {string} */
            status: "invited" | "active" | "suspended";
            /** @description Keys of the roles the person holds, e.g. `owner`, `admin`, `member` (ADR 0004). The UI translates built-in roles by key. */
            roles: string[];
            /**
             * @description Path of the avatar image (`GET /api/v1/people/{personId}/avatar`), if the person has one.
             * @example /api/v1/people/0192b7a0-0000-7000-8000-000000000000/avatar
             */
            avatarUrl?: string;
        };
        PersonList: {
            items: components["schemas"]["Person"][];
        };
        DemoInfo: {
            /**
             * @description The dataset identifier.
             * @example acme
             */
            dataset: string;
            /**
             * @description The demo company name.
             * @example Acme Robotics
             */
            company: string;
            /**
             * @description Who `POST /api/v1/demo/sign-in` signs in as without a username (the Owner).
             * @example maya.okafor
             */
            defaultUsername: string;
            /** @description The people one can sign in as, in dataset order. */
            people: components["schemas"]["DemoPerson"][];
        };
        DemoPerson: {
            /** @example maya.okafor */
            username: string;
            /** @example Maya Okafor */
            displayName: string;
            /** @example Chief Executive Officer */
            title?: string;
            /**
             * @description The key of the person's built-in role (`owner`, `admin` or `member`).
             * @example owner
             */
            role: string;
        };
        DemoSignInRequest: {
            /**
             * @description The demo person to sign in as. Defaults to `defaultUsername` of `GET /api/v1/demo`.
             * @example maya.okafor
             */
            username?: string;
        };
    };
    responses: {
        /** @description The request is invalid. */
        BadRequest: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description The resource does not exist (or belongs to someone else). */
        NotFound: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description The request conflicts with the current state. */
        Conflict: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description Too many attempts; retry after the given number of seconds. */
        TooManyRequests: {
            headers: {
                "Retry-After"?: number;
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /** @description The request has no valid credentials. */
        Unauthorized: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/problem+json": components["schemas"]["Problem"];
            };
        };
        /**
         * @description The caller lacks the permission this operation requires, or a
         *     cookie-authenticated request failed the CSRF checks.
         */
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
    getOrglaneDiscovery: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The discovery document. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["OrglaneDiscovery"];
                };
            };
        };
    };
    getSetupStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The setup status. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SetupStatus"];
                };
            };
            500: components["responses"]["InternalError"];
        };
    };
    completeSetup: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SetupRequest"];
            };
        };
        responses: {
            /** @description The owner was created and signed in. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SignInResult"];
                };
            };
            400: components["responses"]["BadRequest"];
            403: components["responses"]["Forbidden"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["InternalError"];
        };
    };
    signInWithPassword: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["PasswordSignInRequest"];
            };
        };
        responses: {
            /** @description Signed in. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SignInResult"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["InternalError"];
        };
    };
    beginPasskeySignIn: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The WebAuthn options. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PasskeyCeremony"];
                };
            };
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["InternalError"];
        };
    };
    signInWithPasskey: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["PasskeySignInRequest"];
            };
        };
        responses: {
            /** @description Signed in. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SignInResult"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["InternalError"];
        };
    };
    signOut: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Signed out. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    getMe: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The caller. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Me"];
                };
            };
            401: components["responses"]["Unauthorized"];
            500: components["responses"]["InternalError"];
        };
    };
    listMySessions: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The sessions. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SessionList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            500: components["responses"]["InternalError"];
        };
    };
    revokeMySession: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                sessionId: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Revoked. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    listMyPasskeys: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The passkeys. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PasskeyList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            500: components["responses"]["InternalError"];
        };
    };
    addMyPasskey: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AddPasskeyRequest"];
            };
        };
        responses: {
            /** @description The passkey was added. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Passkey"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    beginPasskeyRegistration: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The WebAuthn options. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PasskeyCeremony"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    removeMyPasskey: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                passkeyId: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Removed. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    dismissPasskeyOffer: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Dismissed. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    listPeople: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The people. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PersonList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            500: components["responses"]["InternalError"];
        };
    };
    getPersonAvatar: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                personId: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The avatar image. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "image/svg+xml": string;
                    "image/png": string;
                    "image/webp": string;
                };
            };
            401: components["responses"]["Unauthorized"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    getDemo: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The demo dataset. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DemoInfo"];
                };
            };
            500: components["responses"]["InternalError"];
        };
    };
    signInAsDemoPerson: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: {
            content: {
                "application/json": components["schemas"]["DemoSignInRequest"];
            };
        };
        responses: {
            /** @description Signed in. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SignInResult"];
                };
            };
            400: components["responses"]["BadRequest"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["TooManyRequests"];
            500: components["responses"]["InternalError"];
        };
    };
}
