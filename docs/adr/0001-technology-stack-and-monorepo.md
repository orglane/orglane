# 0001. Technology stack and monorepo

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Orglane is built by a very small team with AI coding agents running around the clock. The stack must be well known to both humans and models, light at runtime, and able to reach web, iOS, Android and, later, desktop quickly.

## Decision

- **Backend:** Go, a single binary with modes (`server`, `agent`, `cli`).
- **Database:** PostgreSQL.
- **Web:** React + Vite SPA (TanStack Router/Query, shadcn/ui), **embedded into the Go binary**, with no Node.js server at runtime.
- **Mobile:** Expo (React Native) with NativeWind and shadcn-style components, sharing design tokens with the web.
- **Desktop:** later; Tauri wrapping the web build is the leading candidate.
- **Shared TypeScript packages:** an API client generated from OpenAPI, query hooks, i18n catalogs, design tokens, the module component kit and its renderers.
- **One public monorepo** for the product. A separate private repository for the cloud control plane only.

## Consequences

- One language per tier, and the most widely known ecosystems for agents.
- Self-hosted installs ship a single core container.
- The web and mobile UIs are two implementations sharing logic, types and tokens. Agents make that affordable.

## Alternatives considered

- **Next.js:** SSR isn't needed behind authentication, and it adds a Node runtime to every install.
- **Flutter everywhere:** weak for dense admin UIs on the web, and less familiar to agents than TypeScript.
- **React Native Web for everything:** painful for table-heavy admin screens.
