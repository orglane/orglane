import { createContext, type ReactNode, useContext, useEffect, useMemo, useState } from "react";
import { IntlProvider } from "react-intl";

import en from "./locales/en.json";
import { defaultLocale, negotiateLocale } from "./negotiate";

/**
 * UI translations (ICU MessageFormat through FormatJS / react-intl).
 *
 * Messages are declared in code with an explicit id and the English text
 * (`<FormattedMessage id="..." defaultMessage="..." />`, `defineMessages`).
 * `pnpm --filter @orglane/web i18n:extract` collects them into
 * `locales/en.json`; adding a language is adding `locales/<tag>.json` with
 * the same ids. `make lint` fails on stale or inconsistent catalogs and on
 * hard-coded text in JSX.
 */

type Catalog = Record<string, string>;

// English is bundled; other catalogs load on demand.
const catalogs = import.meta.glob<{ default: Catalog }>(["./locales/*.json", "!./locales/en.json"]);

/** Locales with a catalog, e.g. ["en"]. */
export const availableLocales: readonly string[] = [
  defaultLocale,
  ...Object.keys(catalogs)
    .map((path) => /\/([^/]+)\.json$/.exec(path)?.[1] ?? "")
    .filter(Boolean),
].sort();

/** Remembers the language the user picked; otherwise the browser decides. */
export const localeStorageKey = "orglane.locale";

function storedLocale(): string | null {
  try {
    return localStorage.getItem(localeStorageKey);
  } catch {
    return null;
  }
}

export function initialLocale(
  stored: string | null = storedLocale(),
  browser: readonly string[] = typeof navigator === "undefined" ? [] : navigator.languages,
): string {
  return negotiateLocale(stored ? [stored, ...browser] : browser, availableLocales);
}

async function loadCatalog(locale: string): Promise<Catalog> {
  if (locale === defaultLocale) {
    return en;
  }
  const load = catalogs[`./locales/${locale}.json`];
  return load ? (await load()).default : en;
}

interface LocaleState {
  locale: string;
  /** null clears the choice and follows the browser again. */
  setLocale(locale: string | null): void;
  available: readonly string[];
}

const LocaleContext = createContext<LocaleState | null>(null);

export function I18nProvider({
  children,
  onLocaleChange,
}: {
  children: ReactNode;
  /** Called with every locale in use, e.g. to send it as Accept-Language. */
  onLocaleChange?: (locale: string) => void;
}) {
  const [locale, setLocaleState] = useState(() => initialLocale());
  const [messages, setMessages] = useState<{ locale: string; catalog: Catalog }>({
    locale: defaultLocale,
    catalog: en,
  });

  useEffect(() => {
    let active = true;
    document.documentElement.lang = locale;
    onLocaleChange?.(locale);
    void loadCatalog(locale).then((catalog) => {
      if (active) {
        setMessages({ locale, catalog });
      }
    });
    return () => {
      active = false;
    };
  }, [locale, onLocaleChange]);

  const value = useMemo<LocaleState>(
    () => ({
      locale,
      available: availableLocales,
      setLocale(next) {
        try {
          if (next) {
            localStorage.setItem(localeStorageKey, next);
          } else {
            localStorage.removeItem(localeStorageKey);
          }
        } catch {
          // Applies to this tab only.
        }
        setLocaleState(next ? negotiateLocale([next], availableLocales) : initialLocale(null));
      },
    }),
    [locale],
  );

  return (
    <LocaleContext.Provider value={value}>
      <IntlProvider
        locale={messages.locale}
        messages={messages.catalog}
        defaultLocale={defaultLocale}
        onError={(err) => {
          // A missing translation falls back to English; that is expected
          // while a catalog is incomplete.
          if (err.code !== "MISSING_TRANSLATION" && import.meta.env.DEV) {
            console.error(err);
          }
        }}
      >
        {children}
      </IntlProvider>
    </LocaleContext.Provider>
  );
}

export function useLocale(): LocaleState {
  const ctx = useContext(LocaleContext);
  if (!ctx) {
    throw new Error("useLocale outside I18nProvider");
  }
  return ctx;
}

/** The name of a language in that language, e.g. "English", "Deutsch". */
export function languageName(locale: string): string {
  try {
    const name = new Intl.DisplayNames([locale], { type: "language" }).of(locale);
    return name ? name.charAt(0).toLocaleUpperCase(locale) + name.slice(1) : locale;
  } catch {
    return locale;
  }
}
