/**
 * The raw color palette. Components never use these directly: they use the
 * semantic colors of a theme (see `themes.ts`), so that light and dark mode
 * (and, later, a company's brand color) only change the mapping.
 *
 * Values are hex so that every platform can use them as they are (CSS, React
 * Native, native code).
 */

export const palette = {
  white: "#ffffff",
  black: "#000000",
  /** A cool neutral gray, used for text, surfaces and borders. */
  neutral: {
    25: "#fcfcfd",
    50: "#f7f7f8",
    100: "#f0f0f2",
    150: "#e9e9ec",
    200: "#e2e2e7",
    300: "#d0d0d7",
    400: "#a0a0ab",
    450: "#868691",
    500: "#71717c",
    600: "#56565f",
    700: "#3f3f47",
    750: "#34343b",
    800: "#28282e",
    850: "#1f1f24",
    900: "#18181c",
    925: "#131316",
    950: "#0e0e11",
    975: "#0a0a0c",
  },
  /** Orglane's brand color, a blue-violet ("iris"). */
  iris: {
    50: "#eff0ff",
    100: "#e1e3ff",
    200: "#c8ccff",
    300: "#a6abfe",
    400: "#8388fa",
    500: "#6866f2",
    600: "#5248e0",
    700: "#4539c4",
    800: "#382f9d",
    900: "#2f297c",
    950: "#1c1848",
  },
  red: {
    50: "#fef2f2",
    100: "#fee2e2",
    300: "#fca5a5",
    400: "#f87171",
    500: "#ef4444",
    600: "#dc2626",
    700: "#b91c1c",
    900: "#7f1d1d",
    950: "#2b1010",
  },
  green: {
    50: "#effdf4",
    100: "#dcfce7",
    300: "#86efac",
    400: "#4ade80",
    600: "#16a34a",
    700: "#15803d",
    900: "#14532d",
    950: "#0c2415",
  },
  amber: {
    50: "#fffbeb",
    100: "#fef3c7",
    300: "#fcd34d",
    400: "#fbbf24",
    600: "#d97706",
    700: "#b45309",
    900: "#78350f",
    950: "#2b1a06",
  },
} as const;
