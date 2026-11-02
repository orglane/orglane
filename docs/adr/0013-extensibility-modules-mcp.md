# 0013. Modules = MCP server + manifest, native component kit

- **Status:** Accepted (direction; the SDK specifics are defined with the public SDK in v0.2)
- **Date:** 2026-11-03

## Context

Companies want to add their own functionality and AI agents on top of company data. Third-party UIs in many platforms are slow and ugly (for example, typical webview mini-apps). Orglane must stay excellent everywhere.

## Decision

- A **module is defined by its manifest**: identity, requested **scopes**, extension points. It isn't defined by its form.
- **A module is an MCP server plus an Orglane manifest**, optionally with UI. The core exposes company data over MCP (read-only in v0.1) with scopes, consent and audit.
- **Access types:** *delegated* (acts as the user, sees only what the user sees) and *module-level* (admin-approved scopes). Every access is audited. Sensitive scopes need admin approval.
- **UI tiers:**
  1. **Native component kit, which is the default (the Raycast model):** List, Detail, Form, Table, Chart, Card, Actions. Modules describe UI and every client renders it natively in Orglane's design. AI-generated UI later uses the same kit.
  2. **Web mini-apps for exceptional cases:** sandboxed webviews with the Orglane SDK (theme, fonts, chrome), cached bundles and a performance budget.
  3. **External services:** "Sign in with Orglane" plus the API.
- **First extension points:** global search, Home widgets, notification sources. More are added later (actions on objects, profile tabs, automations, full screens).
- Built-in features (people, news, Home widgets) are built on the same SDK where practical (dogfooding).
- **Official catalog:** only verified modules (UX and performance reviewed). Unverified modules can be installed by admins and are labeled.
- Prefer emerging standards (MCP, MCP Apps, declarative agent UI) over inventing a plugin protocol. Verify their maturity at SDK design time.

## Consequences

- A consistent, fast UI across third-party modules.
- AI agents become first-class consumers of company data, governed by the same permissions.
- Some exotic UIs need the mini-app tier.
