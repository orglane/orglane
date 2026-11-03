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
    "/api/v1/people/{personId}/teams": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * A person's teams
         * @description The teams a person belongs to, with their role in each, sorted by
         *     name. Archived teams are not included. Any signed-in person may read
         *     it.
         */
        get: operations["listPersonTeams"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/teams": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List teams
         * @description The teams with their member counts, sorted by name. Archived teams
         *     are included only with `archived=true`. Any signed-in person may read
         *     it. Not paginated yet.
         */
        get: operations["listTeams"];
        put?: never;
        /** Create a team */
        post: operations["createTeam"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/teams/{teamId}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * A team
         * @description Returns a team, archived or not. Any signed-in person may read it.
         */
        get: operations["getTeam"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /**
         * Change a team
         * @description Changes the given fields; omitted fields stay unchanged. Archived
         *     teams cannot be changed (409).
         */
        patch: operations["updateTeam"];
        trace?: never;
    };
    "/api/v1/teams/{teamId}/archive": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Archive a team
         * @description Archives a team. It keeps its slug, members and history, becomes
         *     read-only and no longer appears in team lists (unless asked for) or
         *     a person's teams. Archiving an archived team changes nothing. A team
         *     with active child teams cannot be archived (409).
         */
        post: operations["archiveTeam"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/teams/{teamId}/members": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * A team's members
         * @description The members of a team, leads first, then by display name. Offboarded
         *     people are not listed. Archived teams keep their members. Any
         *     signed-in person may read it.
         */
        get: operations["listTeamMembers"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/teams/{teamId}/members/{personId}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * Add a member or set their role
         * @description Adds the person to the team with the given role, or changes the role
         *     of a member: `lead` makes them a team lead, `member` makes a lead a
         *     regular member again. A team may have any number of leads. Setting
         *     the role a member already has changes nothing. Offboarded people
         *     cannot be added, and archived teams cannot change (409).
         */
        put: operations["setTeamMember"];
        post?: never;
        /**
         * Remove a member
         * @description Removes the person from the team. Removing someone who is not a
         *     member changes nothing. Archived teams cannot change (409).
         */
        delete: operations["removeTeamMember"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/people/{personId}/profile": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                personId: string;
            };
            cookie?: never;
        };
        /**
         * A person's profile fields
         * @description The person's profile fields as the caller may see them (ADR 0015), by
         *     section and position: every field the caller may see, with its value
         *     when set and visible to the caller. The rules are applied by the
         *     server for every response; clients never filter.
         *
         *     - Visibility `everyone`: every signed-in person; `team`: people who
         *       share a team with the person; `admins`: only holders of
         *       `people.manage`. The person sees their own non-sensitive fields.
         *     - Sensitive fields are listed only for holders of
         *       `people.sensitive.read`; every response containing a sensitive
         *       value is recorded in the audit log.
         *     - Values the person hid are shown only to them and holders of
         *       `people.manage`.
         *     - Non-sensitive birthdays show day and month (`--MM-DD`) to everyone
         *       except the person and holders of `people.sensitive.read`.
         *
         *     Offboarded people are not found, except by holders of
         *     `people.manage`.
         */
        get: operations["getPersonProfile"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /**
         * Change a person's profile fields
         * @description Sets, clears, hides or shows values of the person's profile fields,
         *     all or nothing, and returns the profile as `GET` does. Only the
         *     person and holders of `people.manage` change a profile: the person
         *     changes fields editable by `self` and hides fields that allow it;
         *     holders of `people.manage` change every field they can see. Values
         *     are validated by type (see `ProfileFieldValueChange`). The audit log
         *     records which fields changed, never the values.
         */
        patch: operations["updatePersonProfile"];
        trace?: never;
    };
    "/api/v1/profile-fields": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List profile field definitions
         * @description The profile field definitions by section and position. Archived
         *     fields only with `includeArchived=true`.
         */
        get: operations["listProfileFields"];
        put?: never;
        /** Create a custom profile field */
        post: operations["createProfileField"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/profile-fields/{fieldId}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /**
         * Change a profile field definition
         * @description Changes the given settings; the key and type never change, and
         *     library fields also keep their label and options. Making a field
         *     sensitive encrypts its existing values (and back). Archiving hides
         *     the field and its values everywhere; restoring brings them back.
         *     Removing a select option that is someone's value answers 409.
         */
        patch: operations["updateProfileField"];
        trace?: never;
    };
    "/api/v1/profile-field-library": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * The library of ready-made profile fields
         * @description Ready-made fields (Telegram, phone, birthday, T-shirt size, ...) with
         *     their default settings and, when turned on before, the field.
         */
        get: operations["listProfileFieldLibrary"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/profile-field-library/{libraryKey}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Turn on a library field
         * @description Creates the field from the library entry with its defaults (201), or
         *     restores it when it was archived (200). Turn it off by archiving the
         *     field (`PATCH /api/v1/profile-fields/{fieldId}`).
         */
        post: operations["enableLibraryProfileField"];
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
            status: components["schemas"]["PersonStatus"];
            /** @description Keys of the roles the person holds, e.g. `owner`, `admin`, `member` (ADR 0004). The UI translates built-in roles by key. */
            roles: string[];
            /**
             * @description Path of the avatar image (`GET /api/v1/people/{personId}/avatar`), if the person has one.
             * @example /api/v1/people/0192b7a0-0000-7000-8000-000000000000/avatar
             */
            avatarUrl?: string;
        };
        /**
         * @description `invited`: added, has not signed in yet; `active`: can sign in; `suspended`: temporarily blocked. Offboarded people are not listed.
         * @enum {string}
         */
        PersonStatus: "invited" | "active" | "suspended";
        PersonList: {
            items: components["schemas"]["Person"][];
        };
        Team: {
            /** Format: uuid */
            id: string;
            /**
             * @description Identifies the team in URLs and, later, in provider mappings. Unique across all teams, archived ones included.
             * @example platform
             */
            slug: string;
            /** @example Platform */
            name: string;
            /**
             * @description What the team does, if set.
             * @example Robot software, firmware and the fleet cloud.
             */
            description?: string;
            /**
             * Format: uuid
             * @description The parent team, if any (prepared for the org chart).
             */
            parentId?: string;
            /**
             * @description How many people are in the team (offboarded people not counted).
             * @example 4
             */
            memberCount: number;
            /**
             * Format: date-time
             * @description When the team was archived. Archived teams are read-only.
             */
            archivedAt?: string;
            /** Format: date-time */
            createdAt: string;
            /** Format: date-time */
            updatedAt: string;
        };
        TeamList: {
            items: components["schemas"]["Team"][];
        };
        CreateTeamRequest: {
            /** @example Platform */
            name: string;
            /**
             * @description 1-63 characters of a-z, 0-9 and `-`, starting and ending with a letter or digit; normalized to lower case.
             * @example platform
             */
            slug: string;
            description?: string;
            /**
             * Format: uuid
             * @description The parent team. It must be an active team.
             */
            parentId?: string;
        };
        /** @description The fields to change; omitted fields stay unchanged. */
        UpdateTeamRequest: {
            name?: string;
            slug?: string;
            /** @description An empty string removes the description. */
            description?: string;
            /** @description The ID of the new parent team (an active team, not this team or one below it), or an empty string to remove the parent. */
            parentId?: string;
        };
        SetTeamMemberRequest: {
            role: components["schemas"]["TeamRole"];
        };
        /**
         * @description A person's role in a team. A team may have any number of leads, including none.
         * @enum {string}
         */
        TeamRole: "member" | "lead";
        TeamMembership: {
            /** Format: uuid */
            teamId: string;
            /** Format: uuid */
            personId: string;
            role: components["schemas"]["TeamRole"];
            /** Format: date-time */
            joinedAt: string;
        };
        TeamMember: {
            person: components["schemas"]["PersonCard"];
            role: components["schemas"]["TeamRole"];
            /** Format: date-time */
            joinedAt: string;
        };
        TeamMemberList: {
            items: components["schemas"]["TeamMember"][];
        };
        /** @description A person as shown in lists of other things, such as a team's members. */
        PersonCard: {
            /** Format: uuid */
            id: string;
            /** @example priya.raman */
            username: string;
            /** @example Priya Raman */
            displayName: string;
            /** @example Senior Robotics Engineer */
            title?: string;
            status: components["schemas"]["PersonStatus"];
            /** @description Path of the avatar image, if the person has one. */
            avatarUrl?: string;
        };
        PersonTeam: {
            team: components["schemas"]["Team"];
            role: components["schemas"]["TeamRole"];
        };
        PersonTeamList: {
            items: components["schemas"]["PersonTeam"][];
        };
        /**
         * @description How values are validated and stored: `text` (one line, at most 200
         *     characters), `long_text` (at most 2000 characters), `date`
         *     (`YYYY-MM-DD`), `select` (an option key), `phone` (E.164, such as
         *     `+442079460958`), `url` (http or https, at most 2000 Unicode
         *     characters), `handle` (an account name
         *     on the service of `handleKind`, without `@`; Mastodon:
         *     `user@instance`).
         * @enum {string}
         */
        ProfileFieldType: "text" | "long_text" | "date" | "select" | "phone" | "url" | "handle";
        /** @enum {string} */
        ProfileFieldHandleKind: "telegram" | "github" | "gitlab" | "x" | "mastodon";
        /** @enum {string} */
        ProfileFieldSection: "contacts" | "personal" | "hr" | "custom";
        /**
         * @description Who sees the values besides the person and holders of
         *     `people.manage`: every signed-in person, people sharing a team with
         *     the person, or nobody else.
         * @enum {string}
         */
        ProfileFieldVisibility: "everyone" | "team" | "admins";
        /**
         * @description `self`: the person and holders of `people.manage`; `admins`: holders of `people.manage` only.
         * @enum {string}
         */
        ProfileFieldEditor: "self" | "admins";
        ProfileFieldOption: {
            /**
             * @description Stored as the value. Derived from the label when omitted on create.
             * @example berlin
             */
            key: string;
            /** @example Berlin */
            label: string;
        };
        ProfileField: {
            /** Format: uuid */
            id: string;
            /** @example telegram */
            key: string;
            /** @description True for fields from the library. Their key is the library key; clients translate the label by it. */
            library: boolean;
            /**
             * @description The English label (library fields) or the admin's label (custom fields).
             * @example Telegram
             */
            label: string;
            type: components["schemas"]["ProfileFieldType"];
            handleKind?: components["schemas"]["ProfileFieldHandleKind"];
            /** @description The choices of a select field; empty for other types. */
            options: components["schemas"]["ProfileFieldOption"][];
            section: components["schemas"]["ProfileFieldSection"];
            visibility: components["schemas"]["ProfileFieldVisibility"];
            requiredAtOnboarding: boolean;
            editableBy: components["schemas"]["ProfileFieldEditor"];
            /** @description The person may hide their value from everyone except holders of `people.manage`. */
            userCanHide: boolean;
            /** @description Values are encrypted at rest, readable only by holders of `people.sensitive.read`, and every read is audited. */
            sensitive: boolean;
            /** @description Order within the section, ascending. */
            position: number;
            archived: boolean;
        };
        ProfileFieldList: {
            items: components["schemas"]["ProfileField"][];
        };
        NewProfileField: {
            /**
             * @description Lower case a-z, 0-9 and `_`, starting with a letter. Derived from the label when omitted. Library keys are reserved.
             * @example office
             */
            key?: string;
            /** @example Office */
            label: string;
            type: components["schemas"]["ProfileFieldType"];
            handleKind?: components["schemas"]["ProfileFieldHandleKind"];
            /** @description Required for select fields. */
            options?: components["schemas"]["NewProfileFieldOption"][];
            section?: components["schemas"]["ProfileFieldSection"];
            visibility?: components["schemas"]["ProfileFieldVisibility"];
            requiredAtOnboarding?: boolean;
            editableBy?: components["schemas"]["ProfileFieldEditor"];
            userCanHide?: boolean;
            sensitive?: boolean;
        };
        NewProfileFieldOption: {
            /** @description Derived from the label when omitted. */
            key?: string;
            label: string;
        };
        /** @description The settings to change; omitted ones stay. Declassifying sensitive values or changing their select options also requires people.sensitive.read, and reads of existing sensitive values are audited. */
        ProfileFieldUpdate: {
            label?: string;
            /** @description Replaces the options of a select field. Keep an option's key to keep its values. */
            options?: components["schemas"]["NewProfileFieldOption"][];
            section?: components["schemas"]["ProfileFieldSection"];
            visibility?: components["schemas"]["ProfileFieldVisibility"];
            requiredAtOnboarding?: boolean;
            editableBy?: components["schemas"]["ProfileFieldEditor"];
            userCanHide?: boolean;
            sensitive?: boolean;
            position?: number;
            archived?: boolean;
        };
        ProfileFieldLibraryEntry: {
            /** @example birthday */
            key: string;
            /**
             * @description The English label; clients translate it by key.
             * @example Birthday
             */
            label: string;
            type: components["schemas"]["ProfileFieldType"];
            handleKind?: components["schemas"]["ProfileFieldHandleKind"];
            options: components["schemas"]["ProfileFieldOption"][];
            section: components["schemas"]["ProfileFieldSection"];
            visibility: components["schemas"]["ProfileFieldVisibility"];
            editableBy: components["schemas"]["ProfileFieldEditor"];
            userCanHide: boolean;
            sensitive: boolean;
            field?: components["schemas"]["ProfileField"];
        };
        ProfileFieldLibrary: {
            items: components["schemas"]["ProfileFieldLibraryEntry"][];
        };
        ProfileFieldValue: {
            field: components["schemas"]["ProfileField"];
            /**
             * @description The value, absent when unset or hidden from the caller. Dates are
             *     `YYYY-MM-DD`; a birthday without its year is `--MM-DD`. Select
             *     fields hold an option key.
             * @example --03-14
             */
            value?: string;
            /** @description The person hid the value from others (reported only to those who still see it). */
            hidden: boolean;
            /** @description The value is a birthday shown without its year. */
            yearHidden: boolean;
            /** @description The caller may set or clear the value. */
            editable: boolean;
            /** @description The caller (the person) may hide the value. */
            hideable: boolean;
        };
        PersonProfile: {
            /** Format: uuid */
            personId: string;
            fields: components["schemas"]["ProfileFieldValue"][];
        };
        ProfileFieldValueChange: {
            /**
             * @description The field key.
             * @example telegram
             */
            key: string;
            /**
             * @description The new value; an empty string clears it; omit to leave it.
             *     Validated by the field type (see `ProfileFieldType`) and stored
             *     normalized (for example a phone number in E.164, a handle
             *     without `@`).
             * @example maya_okafor
             */
            value?: string;
            /** @description Hide (true) or show (false) the value; only the person, and hiding only when the field allows it. */
            hidden?: boolean;
        };
        PersonProfileUpdate: {
            values: components["schemas"]["ProfileFieldValueChange"][];
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
    parameters: {
        TeamId: string;
        PersonId: string;
    };
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
    listPersonTeams: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                personId: components["parameters"]["PersonId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The person's teams. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PersonTeamList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    listTeams: {
        parameters: {
            query?: {
                /** @description Include archived teams. */
                archived?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The teams. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TeamList"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            500: components["responses"]["InternalError"];
        };
    };
    createTeam: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["CreateTeamRequest"];
            };
        };
        responses: {
            /** @description The team was created. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Team"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    getTeam: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The team. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Team"];
                };
            };
            401: components["responses"]["Unauthorized"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    updateTeam: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["UpdateTeamRequest"];
            };
        };
        responses: {
            /** @description The changed team. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Team"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    archiveTeam: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The archived team. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Team"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    listTeamMembers: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The members. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TeamMemberList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    setTeamMember: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
                personId: components["parameters"]["PersonId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SetTeamMemberRequest"];
            };
        };
        responses: {
            /** @description The membership. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TeamMembership"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    removeTeamMember: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                teamId: components["parameters"]["TeamId"];
                personId: components["parameters"]["PersonId"];
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
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    getPersonProfile: {
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
            /** @description The profile. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PersonProfile"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    updatePersonProfile: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                personId: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["PersonProfileUpdate"];
            };
        };
        responses: {
            /** @description The changed profile. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PersonProfile"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            500: components["responses"]["InternalError"];
        };
    };
    listProfileFields: {
        parameters: {
            query?: {
                includeArchived?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The fields. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileFieldList"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    createProfileField: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NewProfileField"];
            };
        };
        responses: {
            /** @description The field was created. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileField"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    updateProfileField: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                fieldId: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProfileFieldUpdate"];
            };
        };
        responses: {
            /** @description The changed field. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileField"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            500: components["responses"]["InternalError"];
        };
    };
    listProfileFieldLibrary: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The library. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileFieldLibrary"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            500: components["responses"]["InternalError"];
        };
    };
    enableLibraryProfileField: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                libraryKey: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The field existed and is turned on. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileField"];
                };
            };
            /** @description The field was created. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProfileField"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
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
