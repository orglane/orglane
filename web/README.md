# web

The Orglane web UI: a React + Vite single-page app, embedded into the core binary and served by `orglane server` ([ADR 0001](../docs/adr/0001-technology-stack-and-monorepo.md)). Licensed under AGPL-3.0-or-later.

| | |
|---|---|
| Routing | [TanStack Router](https://tanstack.com/router) (code-based routes in `src/router.tsx`) |
| Data | [TanStack Query](https://tanstack.com/query) through [`@orglane/api-client`](../packages/api-client): the same public API every client uses |
| Components | [shadcn/ui](https://ui.shadcn.com) style components on [Radix](https://www.radix-ui.com) primitives (`src/components/ui`), [Tailwind CSS v4](https://tailwindcss.com), [Lucide](https://lucide.dev) icons |
| Design tokens | [`@orglane/ui-tokens`](../packages/ui-tokens): CSS variables mapped into Tailwind in `src/styles.css`; light and dark themes |
| i18n | [FormatJS / react-intl](https://formatjs.github.io) with ICU MessageFormat (`src/i18n`) |
| Tests | Vitest + Testing Library (`src/**/*.test.tsx`), Playwright end-to-end (`e2e/`) |

## Screens

First-run setup (`/setup`), sign-in (`/sign-in`, password and passkey), the passkey offer (`/welcome/passkey`), the app shell with Home, Inbox, People and Apps, Sign-in & security (`/settings/security`: passkeys and sessions) and the profile menu (theme, language, sign-out). Screenshots: [`docs/screenshots`](../docs/screenshots).

## Serving and security

The core embeds `web/dist` (`web/web.go`) and serves it from `core/internal/webui`: hashed files under `/assets/` are cached for a year (`immutable`), `index.html` and the other files are revalidated (`no-cache` with an ETag), and every path that is not a file, the API or OIDC gets `index.html` (SPA fallback). The Content-Security-Policy allows only the workspace origin, with no inline scripts or styles, so:

- the theme is applied before the first paint by `public/theme-init.js`, a file rather than an inline script;
- menus are non-modal (`modal={false}`): modal Radix overlays inject `<style>` elements, which the policy refuses;
- nothing is loaded from other hosts: no CDNs, no web fonts (the UI uses the platform's system font).

Without a build, `web/dist` holds only `.gitkeep` and the server answers with a notice instead of the UI, so Go builds and tests work without Node.js.

### Sign-in for applications (OIDC)

`/oidc/authorize` sends the browser to `/oidc/login`, which completes the request at once with a suitable session, or redirects to `/sign-in?authRequest=<id>`. The sign-in screen signs in through the API and returns to `/oidc/login`, which completes the request (no passkey offer in between). Browsers that cannot run the app (no JavaScript modules, such as the OpenID Conformance Suite's HtmlUnit) get a basic password form instead: it is hidden in `index.html`, revealed only by `public/basic-sign-in.js` (`<script nomodule>`), and posts to `/oidc/login`. Its text comes from the English catalog at build time.

## Sessions

`SessionGuard` (`src/lib/session-guard.tsx`) owns the one transition from signed in to signed out. Signing out, revoking this session, a refetch of the current person (`GET /api/v1/me`) that finds no session (for example after returning to a tab whose session expired or was revoked elsewhere), and a 401 from any other request all end there. It clears the session's cached data and the CSRF token and goes to `/sign-in`. After an expiry it shows "Your session has ended" and returns to the page afterwards. The signed-in screens unmount as soon as the current person is gone, so nothing renders without a session.

## Design tokens and themes

Colors, typography, spacing, radii and shadows live in `packages/ui-tokens` (shared with the future mobile app). `tokens.css` defines them as `--orglane-*` CSS variables; `src/styles.css` maps them into Tailwind (`bg-primary`, `text-muted-foreground`, `rounded-lg`, `shadow-md`, ...). The theme follows `prefers-color-scheme`; the profile menu (and the toggle on the sign-in screens) can force light or dark, stored in `localStorage`. Every text and background pair meets WCAG AA contrast, which the `ui-tokens` tests check.

## i18n

Every user-facing string is an ICU message with an explicit, namespaced id, declared next to the code that uses it:

```tsx
const messages = defineMessages({ title: { id: "inbox.title", defaultMessage: "Inbox" } });
const greeting = defineMessage<{ name: string }>({ id: "home.hello", defaultMessage: "Hello, {name}" });

<FormattedMessage {...messages.title} />
intl.formatMessage(greeting, { name });
```

- `pnpm --filter @orglane/web i18n:extract` (or `make generate`) collects the messages into `src/i18n/locales/en.json`.
- `pnpm --filter @orglane/web i18n:check` (part of `make lint`) fails when `en.json` is stale, an id is not namespaced, or another catalog has unknown ids, invalid ICU syntax or different placeholders.
- Biome plugins (`web/lint/*.grit`, part of `make lint`) fail on hard-coded text in JSX: text children, string children such as `{"text"}`, and string `aria-label`, `title`, `placeholder`, `alt` and `label` attributes.
- Adding a language is adding `src/i18n/locales/<tag>.json` with (some of) the same ids; missing messages fall back to English. The locale is the user's choice from the profile menu, otherwise the best match for the browser's languages (`navigator.languages`, what the browser sends as `Accept-Language`); the API receives it as `Accept-Language`.

## Development

```sh
pnpm --filter @orglane/web dev          # Vite on http://localhost:5173, proxying the API to :8080
```

Run the core so that it sees the dev server's origin (cookies, the CSRF Origin check and passkeys depend on it):

```sh
ORGLANE_DOMAIN_WORKSPACE_HOST=localhost:5173 make dev
```

`make build` builds the UI and then the binary that embeds it; `make dev` alone serves whatever was built last.

## Tests

```sh
pnpm --filter @orglane/web test         # Vitest (part of make test)
make test-e2e                           # Playwright: needs Docker and Playwright's Chromium
```

The end-to-end tests (`e2e/`) build the binary with the UI, start PostgreSQL in Docker and give every test its own database and `orglane server` on localhost. They cover first-run setup → passkey offer → Skip → shell → sign-out → sign-in, passkey registration and sign-in with Chromium's virtual WebAuthn authenticator (CDP), sign-in for an OIDC application, a session revoked elsewhere while a page is open (focus refetch → sign-in → back), caching and security headers, and that no request leaves the origin or violates the CSP. `make test` runs them when Docker and Chromium are available (`pnpm --filter @orglane/web exec playwright install chromium`) and prints a warning otherwise.

`ORGLANE_SCREENSHOTS=1 pnpm --filter @orglane/web test:e2e screenshots` refreshes the screenshots in `docs/screenshots`.
