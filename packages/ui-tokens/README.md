# @orglane/ui-tokens

Design tokens shared by the Orglane web and mobile apps. Licensed under Apache-2.0 (see [`../LICENSE`](../LICENSE)).

- `palette`: the raw colors (hex, so every platform can use them).
- `themes.light`, `themes.dark`: semantic colors (`background`, `foreground`, `primary`, `mutedForeground`, `destructiveSubtle`, ...). Components use these, never the palette. Every `*Foreground` color meets WCAG AA contrast on its background, and focus rings 3:1; the tests check it.
- `typography`, `spacing` (a 4px grid), `radii`, `shadows` (per theme, as layers), `durations`.
- `tokens.css`: all of the above as `--orglane-*` CSS custom properties. The light theme is the default, the dark theme follows `prefers-color-scheme`, and the classes `light` / `dark` on the root element force one. The web app maps them into Tailwind (`web/src/styles.css`).

`tokens.css` is generated from `src/` (`pnpm --filter @orglane/ui-tokens generate`, part of `make generate`); `make lint` and the tests fail when it is stale.
