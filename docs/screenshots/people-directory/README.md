# People directory and profiles

Issue #25 review evidence, captured by `web/e2e/people.spec.ts` against the real
embedded web app, demo server and PostgreSQL. The signed-in caller is the demo
Admin Lucas Ferreira; the profile is Priya Raman with Maya Okafor as manager and
a public Telegram field configured through the public API.

Each directory/profile pair has light and dark captures at 1440, 768 and 360px.
Playwright checks that the document does not overflow horizontally at each size.
These show the functional prototype built with the existing components and tokens;
visual polish remains a separate requested follow-up.

Regenerate from the repository root:

```sh
pnpm --filter @orglane/web build
pnpm --filter @orglane/web test:e2e people.spec.ts
```

| Width | Directory | Profile |
| --- | --- | --- |
| 1440 | [Light](directory-1440-light.png) · [Dark](directory-1440-dark.png) | [Light](profile-1440-light.png) · [Dark](profile-1440-dark.png) |
| 768 | [Light](directory-768-light.png) · [Dark](directory-768-dark.png) | [Light](profile-768-light.png) · [Dark](profile-768-dark.png) |
| 360 | [Light](directory-360-light.png) · [Dark](directory-360-dark.png) | [Light](profile-360-light.png) · [Dark](profile-360-dark.png) |
