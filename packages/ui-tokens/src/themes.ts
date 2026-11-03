import { palette as p } from "./palette.ts";

/**
 * Semantic colors. Every UI surface is described by one of these roles; a
 * theme maps each role to a palette value. `*Foreground` is the text and icon
 * color to use on the matching background, and every such pair meets WCAG AA
 * contrast (checked by the tests).
 */
export interface ThemeColors {
  /** The page behind content. */
  background: string;
  foreground: string;
  /** The app frame around content: sidebar, top bar, auth screens. */
  canvas: string;
  canvasForeground: string;
  /** Cards and panels on the background. */
  surface: string;
  surfaceForeground: string;
  /** Menus, popovers and dialogs. */
  popover: string;
  popoverForeground: string;
  /** Quiet fills: inactive tabs, skeletons, badges. */
  muted: string;
  /** Secondary text on background, surface and muted fills. */
  mutedForeground: string;
  /** Hover and selected states of list items and menu entries. */
  accent: string;
  accentForeground: string;
  /** Hairlines between areas and around cards. */
  border: string;
  /**
   * Borders of form controls. Unlike decorative `border`, they identify the
   * control, so they reach 3:1 against every background (WCAG 1.4.11).
   */
  input: string;
  /** Focus rings. */
  ring: string;
  /** The main call to action. */
  primary: string;
  primaryForeground: string;
  /** Tinted brand fills: the active navigation item, highlights. */
  primarySubtle: string;
  primarySubtleForeground: string;
  /** Neutral buttons. */
  secondary: string;
  secondaryForeground: string;
  /** Destructive actions and errors. */
  destructive: string;
  destructiveForeground: string;
  destructiveSubtle: string;
  destructiveSubtleForeground: string;
  /** Confirmations. */
  successSubtle: string;
  successSubtleForeground: string;
  /** Warnings. */
  warningSubtle: string;
  warningSubtleForeground: string;
}

export type ThemeName = "light" | "dark";

export const themes: Record<ThemeName, ThemeColors> = {
  light: {
    background: p.white,
    foreground: p.neutral[900],
    canvas: p.neutral[50],
    canvasForeground: p.neutral[900],
    surface: p.white,
    surfaceForeground: p.neutral[900],
    popover: p.white,
    popoverForeground: p.neutral[900],
    muted: p.neutral[100],
    mutedForeground: p.neutral[600],
    accent: p.neutral[100],
    accentForeground: p.neutral[900],
    border: p.neutral[200],
    input: p.neutral[450],
    ring: p.iris[500],
    primary: p.iris[600],
    primaryForeground: p.white,
    primarySubtle: p.iris[50],
    primarySubtleForeground: p.iris[700],
    secondary: p.neutral[100],
    secondaryForeground: p.neutral[900],
    destructive: p.red[600],
    destructiveForeground: p.white,
    destructiveSubtle: p.red[50],
    destructiveSubtleForeground: p.red[700],
    successSubtle: p.green[50],
    successSubtleForeground: p.green[700],
    warningSubtle: p.amber[50],
    warningSubtleForeground: p.amber[700],
  },
  dark: {
    background: p.neutral[925],
    foreground: p.neutral[100],
    canvas: p.neutral[975],
    canvasForeground: p.neutral[100],
    surface: p.neutral[900],
    surfaceForeground: p.neutral[100],
    popover: p.neutral[850],
    popoverForeground: p.neutral[100],
    muted: p.neutral[850],
    mutedForeground: p.neutral[400],
    accent: p.neutral[800],
    accentForeground: p.neutral[100],
    border: p.neutral[800],
    input: p.neutral[500],
    ring: p.iris[400],
    primary: p.iris[400],
    primaryForeground: p.neutral[975],
    primarySubtle: "#1f1d45",
    primarySubtleForeground: p.iris[200],
    secondary: p.neutral[800],
    secondaryForeground: p.neutral[100],
    destructive: p.red[600],
    destructiveForeground: p.white,
    destructiveSubtle: p.red[950],
    destructiveSubtleForeground: p.red[300],
    successSubtle: p.green[950],
    successSubtleForeground: p.green[300],
    warningSubtle: p.amber[950],
    warningSubtleForeground: p.amber[300],
  },
};
