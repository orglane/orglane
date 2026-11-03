import type { IntlShape } from "react-intl";

const units: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
];

/** "5 minutes ago", "yesterday", "now", in the UI locale. */
export function relativeTime(intl: IntlShape, date: string | Date, now: Date = new Date()): string {
  const seconds = (new Date(date).getTime() - now.getTime()) / 1000;
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size) {
      return intl.formatRelativeTime(Math.round(seconds / size), unit, { numeric: "auto" });
    }
  }
  return intl.formatRelativeTime(0, "second", { numeric: "auto" });
}

/**
 * A timestamp to the minute with the time zone, in the UI locale and the
 * browser's time zone: "Nov 3, 2026, 1:04 PM GMT+3".
 */
export function exactTime(intl: IntlShape, date: string | Date): string {
  return intl.formatDate(date, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short",
  });
}
