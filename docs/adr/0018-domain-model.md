# 0018. Domain model

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Orglane runs self-hosted on a company's own domain and, later, in the cloud for many companies. The project also needs its own landing, docs, sign-in, checkout and push relay. Each company's data must be isolated in the browser (cookies, storage) from other companies and from the project's own sites. The same software must work in every setup without code changes.

## Decision

**1. `orglane.app` hosts only the project's own sites and services.** No company data ever lives there.

| Address | Purpose |
|---|---|
| `orglane.app` | Landing, docs, `install.sh` |
| `orglane.app/login`, `/signup`, `/checkout` | Cloud sign-in, sign-up, payments (Link and Cloud) |
| `orglane.app/<slug>` | Short link that redirects to a company's workspace |
| `orglane.app/open/...` | Universal Links / App Links into the mobile app |
| `push.orglane.app` | Push relay (API only, no cookies) |

Billing UI lives inside each company's own workspace (**Settings → Billing**, **Settings → Orglane Link**) and calls the cloud API. There is no separate dashboard host.

**2. A separate customer domain is used for cloud workspaces and Link tunnels.** It will be acquired together with Link; `orglane.cloud` is the leading candidate. It is registered in the **Public Suffix List** so that each company subdomain is a separate site in browsers.

```
acme.orglane.cloud         workspace
git.acme.orglane.cloud     company services
```

**3. A company's own domain is the recommended setup** for both self-hosted and cloud:

```
work.acme.com (or acme.com)   workspace
git.acme.com                  company services
```

**4. One model in the core:** an instance knows its **company base domain** and the **workspace host**, and builds service hosts from a template (`{service}.{base}` by default). Self-hosted, Link, cloud default and custom domains differ only in configuration.

**Requirements for every instance:**
- No hard-coded hosts. Project service endpoints (push relay, discovery) are configurable, with defaults on `orglane.app`.
- **Session cookies are host-only** (no `Domain` attribute). Services get single sign-on only through OIDC, never through shared cookies.
- **Discovery by email:** self-hosted instances answer at `https://<email-domain>/.well-known/orglane`; cloud instances are found through the discovery API on `orglane.app`. The mobile app supports both from the start.

## Consequences

- Browser isolation holds in every deployment model, including many companies on one shared customer domain.
- Moving between self-hosted, Link and cloud means changing configuration and DNS, not code.
- A second domain has to be bought and submitted to the Public Suffix List well before the Link launch, because PSL review takes weeks.

## Alternatives considered

- **Path-based workspaces on `orglane.app` (`orglane.app/acme`, as in Linear):** every company would share one origin, and paths are not a browser security boundary. Company services would still need their own hosts. Kept only as a redirecting short link.
- **Company subdomains under `orglane.app`:** customer instances could set cookies affecting the project's own sites.
