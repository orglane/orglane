import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { contrastRatio, cssVar, kebab, type ThemeColors, themes, toCss } from "./index.ts";

describe("cssVar", () => {
  it("builds a prefixed CSS variable reference", () => {
    expect(cssVar("radius-md")).toBe("var(--orglane-radius-md)");
  });

  it("rejects names that are not kebab-case", () => {
    expect(() => cssVar("Radius MD")).toThrow(/invalid token name/);
    expect(() => cssVar("")).toThrow(/invalid token name/);
  });
});

describe("contrastRatio", () => {
  it("matches the WCAG reference values", () => {
    expect(contrastRatio("#000000", "#ffffff")).toBeCloseTo(21, 5);
    expect(contrastRatio("#ffffff", "#ffffff")).toBe(1);
    expect(contrastRatio("#777", "#fff")).toBeCloseTo(4.48, 2);
  });
});

// Every text color on the background it is meant for must reach WCAG AA
// (4.5:1 for body text); focus rings and control borders need 3:1 (WCAG 1.4.11).
const textPairs: [keyof ThemeColors, keyof ThemeColors][] = [
  ["foreground", "background"],
  ["canvasForeground", "canvas"],
  ["surfaceForeground", "surface"],
  ["popoverForeground", "popover"],
  ["mutedForeground", "background"],
  ["mutedForeground", "surface"],
  ["mutedForeground", "canvas"],
  ["mutedForeground", "muted"],
  ["accentForeground", "accent"],
  ["primaryForeground", "primary"],
  ["primarySubtleForeground", "primarySubtle"],
  ["secondaryForeground", "secondary"],
  ["destructiveForeground", "destructive"],
  ["destructiveSubtleForeground", "destructiveSubtle"],
  ["successSubtleForeground", "successSubtle"],
  ["warningSubtleForeground", "warningSubtle"],
];

describe.each(Object.entries(themes))("the %s theme", (_name, colors) => {
  it.each(textPairs)("%s on %s meets WCAG AA", (fg, bg) => {
    expect(contrastRatio(colors[fg], colors[bg])).toBeGreaterThanOrEqual(4.5);
  });

  // Non-text contrast (WCAG 1.4.11): focus rings and the borders that
  // identify form controls need 3:1 against everything they sit on.
  it.each(["ring", "input"] as const)("%s is visible on every background (3:1)", (role) => {
    for (const bg of ["background", "canvas", "surface", "popover", "muted"] as const) {
      expect(contrastRatio(colors[role], colors[bg]), `${role} on ${bg}`).toBeGreaterThanOrEqual(3);
    }
  });

  it("defines the same roles as the light theme", () => {
    expect(Object.keys(colors).sort()).toEqual(Object.keys(themes.light).sort());
  });
});

describe("toCss", () => {
  const css = toCss();

  it("emits every semantic color for both themes", () => {
    for (const key of Object.keys(themes.light)) {
      expect(css.match(new RegExp(`--orglane-color-${kebab(key)}:`, "g"))).toHaveLength(3);
    }
  });

  it("follows prefers-color-scheme unless the user picked a theme", () => {
    expect(css).toContain("@media (prefers-color-scheme: dark)");
    expect(css).toContain(":root:not(.light)");
    expect(css).toContain(":root.dark");
  });

  it("matches the committed tokens.css", () => {
    const committed = readFileSync(new URL("../tokens.css", import.meta.url), "utf8");
    expect(committed, "tokens.css is stale, run 'make generate'").toBe(css);
  });
});
