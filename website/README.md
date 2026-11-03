# website

The project's website on **orglane.app**: the landing page and the installer at `orglane.app/install.sh` ([ADR 0018](../docs/adr/0018-domain-model.md): `orglane.app` hosts only the project's own sites and services, never company data). Licensed under AGPL-3.0-or-later.

A static site built with [Astro](https://astro.build) into `website/dist` and deployed to GitHub Pages. It ships no framework runtime: the only script is the copy button of the install commands (progressive enhancement; the page works without JavaScript). No external fonts, trackers, CDNs or cookies: the system font stack and the colors of [`@orglane/ui-tokens`](../packages/ui-tokens), light and dark following `prefers-color-scheme`. Screenshots: [`docs/screenshots/website-*.png`](../docs/screenshots).

## Development

```sh
make website-dev     # live reload on http://localhost:4321
make website         # build into website/dist and check the result
make test-website    # build, then the Playwright tests against `astro preview`
pnpm --filter @orglane/website test   # unit tests (Vitest, part of make test)
```

`make test` runs the Playwright tests when Playwright's Chromium is installed (`pnpm --filter @orglane/web exec playwright install chromium`). `ORGLANE_SCREENSHOTS=1 make test-website` refreshes the screenshots.

| Path | Contents |
|---|---|
| `src/pages/` | Routes: `index.astro` (`/`), `install.sh.ts` (`/install.sh`), `404.astro` |
| `src/components/`, `src/layouts/` | The page sections and the HTML shell (header, footer, meta tags) |
| `src/i18n/` | Every user-facing string (`en.ts`); templates only compose them |
| `src/lib/site.ts` | The site's URL, the repository links, its routes and the paths reserved for later |
| `src/lib/installer.ts` | `deploy/install.sh` and its SHA-256, read at build time |
| `scripts/check-dist.ts` | Runs after every build, see below |
| `public/` | Copied as-is: `CNAME`, `favicon.svg`, `robots.txt` |
| `e2e/` | Playwright tests of the built site |

## The installer

`deploy/install.sh` is the single source: the build serves it unchanged at `/install.sh` and shows its SHA-256 on the page. The installer itself installs the latest GitHub release by default (`--version` picks another).

**`/install.sh` is published from `main`.** That is right while there are no releases: the script on `main` is the only one. Once releases exist, the installer should be published from the latest release tag instead, so the script people pipe into `sh` is the reviewed, released one and matches the release it installs (follow-up: build the site's `/install.sh` from `git show <latest tag>:deploy/install.sh`, and redeploy when a release is published). `scripts/check-dist.ts` fails the build when `dist/install.sh` is not byte-identical to `deploy/install.sh`, the page does not show its checksum, `CNAME` does not match `src/lib/site.ts`, or any file loads a resource from another origin. GitHub Pages serves `.sh` files as `application/x-sh`; after each deployment the workflow fetches `https://orglane.app/install.sh` and compares it with the commit's `deploy/install.sh`.

## Routes and later sections

The site uses directory-style URLs (`/docs/` → `docs/index.html`). ADR 0018 reserves `/docs`, `/login`, `/signup`, `/checkout`, `/open/...` and `orglane.app/<slug>` short links; nothing is built there yet (`reservedRoutes` in `src/lib/site.ts`, checked by a test). Docs can be added as pages under `src/pages/docs/`. Sign-in, sign-up, checkout, short links and app links need a server and will not be served by GitHub Pages; they come with Orglane Cloud and Link, together with the decision where they run.

## Deployment

`.github/workflows/website.yml` runs on pushes to `main` that change `website/`, `deploy/install.sh`, `packages/ui-tokens/`, `pnpm-lock.yaml` or the workflow, and by hand (`gh workflow run website.yml`). It builds with `make website`, uploads `website/dist` as the Pages artifact and deploys it with `actions/deploy-pages`, then checks `/install.sh` on the deployed site.

The deploy job runs because the repository is public. GitHub Free has no Pages for private repositories, so in a private copy the workflow only builds, unless the repository variable `WEBSITE_DEPLOY` is `true` (on a paid plan).

### One-time setup (maintainer)

1. **Verify the domain for the organization** (protects against takeover of the domain by other Pages sites): GitHub → organization **Settings → Pages → Add a domain** → `orglane.app`. GitHub shows a TXT record; add it at Porkbun (below) and click **Verify**.
2. **Enable Pages for the repository:** repository **Settings → Pages → Build and deployment → Source: GitHub Actions**.
3. **Set the custom domain:** **Settings → Pages → Custom domain** → `orglane.app` → **Save**. With an Actions deployment, GitHub ignores `public/CNAME` (it is kept as the record of the intended domain); this setting is what counts.
4. **Add the DNS records** at Porkbun (below) and wait for the DNS check in **Settings → Pages** to pass.
5. **Enforce HTTPS** once GitHub has issued the certificate (**Settings → Pages → Enforce HTTPS**). `.app` is on the HSTS preload list, so browsers open the site only over HTTPS: it is unreachable in browsers until the certificate exists, which usually takes minutes to an hour after DNS resolves.
6. Run the workflow (`gh workflow run website.yml`) and check `curl -fsSL https://orglane.app/install.sh | head -3`.

### DNS records at Porkbun

Porkbun → **Domain Management** → `orglane.app` → **DNS**. First delete Porkbun's default parking records (the `ALIAS` on the apex and the `CNAME` on `*`, both pointing at `pixie.porkbun.com`), then add:

| Type | Host | Answer | Purpose |
|---|---|---|---|
| `A` | *(empty, the apex)* | `185.199.108.153` | GitHub Pages |
| `A` | *(empty)* | `185.199.109.153` | GitHub Pages |
| `A` | *(empty)* | `185.199.110.153` | GitHub Pages |
| `A` | *(empty)* | `185.199.111.153` | GitHub Pages |
| `AAAA` | *(empty)* | `2606:50c0:8000::153` | GitHub Pages (IPv6) |
| `AAAA` | *(empty)* | `2606:50c0:8001::153` | GitHub Pages (IPv6) |
| `AAAA` | *(empty)* | `2606:50c0:8002::153` | GitHub Pages (IPv6) |
| `AAAA` | *(empty)* | `2606:50c0:8003::153` | GitHub Pages (IPv6) |
| `CNAME` | `www` | `orglane.github.io` | `www.orglane.app` redirects to `orglane.app` |
| `TXT` | `_github-pages-challenge-orglane` | *(the value GitHub shows in step 1)* | Domain verification |

The addresses are GitHub's published Pages addresses ([docs](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site#configuring-an-apex-domain)); check them there before adding. Keep any `CAA` records compatible with Let's Encrypt (`0 issue "letsencrypt.org"`), which issues the Pages certificate. Other `orglane.app` hosts from ADR 0018 (such as `push.orglane.app`) get their own records when those services exist.
