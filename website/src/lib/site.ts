// The project's own site (ADR 0018: orglane.app hosts only the project's
// sites and services, never company data). Every absolute URL of the site is
// built from `siteUrl`, so it is set in this one place.

export const siteUrl = "https://orglane.app";

/** The host GitHub Pages serves the site on (public/CNAME holds the same value). */
export const siteHost = new URL(siteUrl).host;

export const repositoryUrl = "https://github.com/orglane/orglane";

/** A file in the repository on its default branch, for example `ROADMAP.md`. */
export function repositoryFile(path: string): string {
  return `${repositoryUrl}/blob/main/${path}`;
}

/** Pages and files built in this site. */
export const routes = {
  home: "/",
  installer: "/install.sh",
} as const;

/**
 * Paths that ADR 0018 reserves on orglane.app for later project services.
 * Nothing is built there yet; the site uses directory-style URLs
 * (`/docs/` → `docs/index.html`), so each can be added as a page or a
 * directory of pages under `src/pages` without moving anything.
 */
export const reservedRoutes = ["/docs", "/login", "/signup", "/checkout", "/open"] as const;

/** The absolute URL of a path on the site. */
export function absoluteUrl(path: string): string {
  return new URL(path, siteUrl).href;
}

/** The URL the install command downloads, as shown on the page. */
export const installerUrl = absoluteUrl(routes.installer);
