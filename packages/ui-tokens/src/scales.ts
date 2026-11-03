/**
 * Non-color tokens. Sizes are in px (density-independent points on mobile);
 * the CSS output converts them to rem with a 16px root.
 */

export const typography = {
  fontFamily: {
    /**
     * The platform's own UI font (San Francisco, Segoe UI, Roboto, ...):
     * native-looking, nothing to download, no font from a CDN.
     */
    sans: [
      "ui-sans-serif",
      "system-ui",
      "-apple-system",
      "BlinkMacSystemFont",
      "Segoe UI",
      "Roboto",
      "Helvetica Neue",
      "Arial",
      "sans-serif",
      "Apple Color Emoji",
      "Segoe UI Emoji",
    ],
    mono: ["ui-monospace", "SFMono-Regular", "SF Mono", "Menlo", "Consolas", "monospace"],
  },
  /** Font size and line height pairs. */
  fontSize: {
    xs: { size: 12, lineHeight: 16 },
    sm: { size: 14, lineHeight: 20 },
    base: { size: 16, lineHeight: 24 },
    lg: { size: 18, lineHeight: 28 },
    xl: { size: 20, lineHeight: 28 },
    "2xl": { size: 24, lineHeight: 32 },
    "3xl": { size: 30, lineHeight: 36 },
  },
  fontWeight: {
    regular: 400,
    medium: 500,
    semibold: 600,
    bold: 700,
  },
  /** Letter spacing in em; headings are set slightly tighter. */
  letterSpacing: {
    tight: -0.015,
    normal: 0,
  },
} as const;

/** Spacing is a 4px grid: spacing `n` is `n * unit`. */
export const spacing = {
  unit: 4,
} as const;

export const radii = {
  xs: 4,
  sm: 6,
  md: 8,
  lg: 12,
  xl: 16,
  full: 9999,
} as const;

/** One layer of a shadow. Mobile platforms use the last (largest) layer. */
export interface ShadowLayer {
  x: number;
  y: number;
  blur: number;
  spread: number;
  color: string;
  opacity: number;
}

export type ShadowName = "xs" | "sm" | "md" | "lg";

const ink = "#101018";

export const shadows: Record<"light" | "dark", Record<ShadowName, ShadowLayer[]>> = {
  light: {
    xs: [{ x: 0, y: 1, blur: 2, spread: 0, color: ink, opacity: 0.05 }],
    sm: [
      { x: 0, y: 1, blur: 2, spread: 0, color: ink, opacity: 0.04 },
      { x: 0, y: 1, blur: 3, spread: 0, color: ink, opacity: 0.08 },
    ],
    md: [
      { x: 0, y: 2, blur: 4, spread: -2, color: ink, opacity: 0.05 },
      { x: 0, y: 4, blur: 12, spread: -2, color: ink, opacity: 0.08 },
    ],
    lg: [
      { x: 0, y: 4, blur: 8, spread: -4, color: ink, opacity: 0.06 },
      { x: 0, y: 16, blur: 32, spread: -8, color: ink, opacity: 0.16 },
    ],
  },
  dark: {
    xs: [{ x: 0, y: 1, blur: 2, spread: 0, color: "#000000", opacity: 0.4 }],
    sm: [
      { x: 0, y: 1, blur: 2, spread: 0, color: "#000000", opacity: 0.3 },
      { x: 0, y: 1, blur: 3, spread: 0, color: "#000000", opacity: 0.4 },
    ],
    md: [
      { x: 0, y: 2, blur: 4, spread: -2, color: "#000000", opacity: 0.4 },
      { x: 0, y: 4, blur: 12, spread: -2, color: "#000000", opacity: 0.5 },
    ],
    lg: [
      { x: 0, y: 4, blur: 8, spread: -4, color: "#000000", opacity: 0.5 },
      { x: 0, y: 16, blur: 32, spread: -8, color: "#000000", opacity: 0.6 },
    ],
  },
};

/** Motion durations in milliseconds. */
export const durations = {
  fast: 120,
  normal: 200,
  slow: 320,
} as const;
