/**
 * Placeholder for the Orglane design tokens. The real token set (colors,
 * typography, spacing, radii) is defined by the web skeleton work; this
 * module only exists so the TypeScript toolchain has something to build,
 * lint and test.
 */

/** Prefix of every CSS custom property emitted for Orglane tokens. */
export const cssVarPrefix = "orglane";

/** Returns the CSS `var()` reference for a token, e.g. `var(--orglane-radius-md)`. */
export function cssVar(token: string): string {
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(token)) {
    throw new Error(`invalid token name: ${JSON.stringify(token)}`);
  }
  return `var(--${cssVarPrefix}-${token})`;
}
