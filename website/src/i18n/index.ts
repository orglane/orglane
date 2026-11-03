import { en, type Messages } from "./en.ts";

export const defaultLocale = "en";

const catalogs: Record<string, Messages> = { en };

/** The catalog for a locale; English for unknown locales. */
export function messages(locale: string = defaultLocale): Messages {
  return catalogs[locale] ?? en;
}
