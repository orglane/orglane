import {
  radii,
  type ShadowLayer,
  type ShadowName,
  shadows,
  spacing,
  typography,
} from "./scales.ts";
import { type ThemeColors, type ThemeName, themes } from "./themes.ts";

/** Prefix of every CSS custom property emitted for Orglane tokens. */
export const cssVarPrefix = "orglane";

const tokenName = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

/** Returns the CSS `var()` reference for a token, e.g. `var(--orglane-radius-md)`. */
export function cssVar(token: string): string {
  if (!tokenName.test(token)) {
    throw new Error(`invalid token name: ${JSON.stringify(token)}`);
  }
  return `var(--${cssVarPrefix}-${token})`;
}

/** `primaryForeground` → `primary-foreground`, `2xl` → `2xl`. */
export function kebab(name: string): string {
  return name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);
}

function rem(px: number): string {
  return `${Number((px / 16).toFixed(4))}rem`;
}

function alpha(hex: string, opacity: number): string {
  const h = hex.replace("#", "");
  const [r, g, b] = [0, 2, 4].map((i) => Number.parseInt(h.slice(i, i + 2), 16));
  return `rgb(${r} ${g} ${b} / ${opacity})`;
}

/** Renders shadow layers as a CSS `box-shadow` value. */
export function shadowCss(layers: readonly ShadowLayer[]): string {
  return layers
    .map((l) => `${l.x}px ${l.y}px ${l.blur}px ${l.spread}px ${alpha(l.color, l.opacity)}`)
    .join(", ");
}

function fontStack(fonts: readonly string[]): string {
  return fonts.map((f) => (/^[a-z-]+$/.test(f) ? f : `"${f}"`)).join(", ");
}

function themeDecls(name: ThemeName): string[] {
  const colors: ThemeColors = themes[name];
  const lines = [`color-scheme: ${name};`];
  for (const [key, value] of Object.entries(colors)) {
    lines.push(`--${cssVarPrefix}-color-${kebab(key)}: ${value};`);
  }
  for (const [key, layers] of Object.entries(shadows[name]) as [ShadowName, ShadowLayer[]][]) {
    lines.push(`--${cssVarPrefix}-shadow-${key}: ${shadowCss(layers)};`);
  }
  return lines;
}

function scaleDecls(): string[] {
  const v = (name: string, value: string | number) => `--${cssVarPrefix}-${name}: ${value};`;
  const lines = [
    v("font-sans", fontStack(typography.fontFamily.sans)),
    v("font-mono", fontStack(typography.fontFamily.mono)),
  ];
  for (const [key, { size, lineHeight }] of Object.entries(typography.fontSize)) {
    lines.push(v(`text-${key}`, rem(size)), v(`text-${key}-line-height`, rem(lineHeight)));
  }
  for (const [key, weight] of Object.entries(typography.fontWeight)) {
    lines.push(v(`font-weight-${key}`, weight));
  }
  for (const [key, em] of Object.entries(typography.letterSpacing)) {
    lines.push(v(`tracking-${key}`, `${em}em`));
  }
  lines.push(v("spacing", rem(spacing.unit)));
  for (const [key, px] of Object.entries(radii)) {
    lines.push(v(`radius-${key}`, px >= 9999 ? "9999px" : rem(px)));
  }
  return lines;
}

function block(selector: string, lines: string[], indent = ""): string {
  return `${indent}${selector} {\n${lines.map((l) => `${indent}  ${l}`).join("\n")}\n${indent}}`;
}

/**
 * Renders all tokens as CSS custom properties. The light theme applies by
 * default and the dark theme follows `prefers-color-scheme`; the classes
 * `light` and `dark` on the root element force a theme (the user's choice).
 */
export function toCss(): string {
  return `${[
    "/* Generated from packages/ui-tokens/src by `pnpm --filter @orglane/ui-tokens generate`. Do not edit. */",
    block(":root", [...scaleDecls(), ...themeDecls("light")]),
    `@media (prefers-color-scheme: dark) {\n${block(":root:not(.light)", themeDecls("dark"), "  ")}\n}`,
    block(":root.dark", themeDecls("dark")),
  ].join("\n\n")}\n`;
}
