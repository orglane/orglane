/** The source language of the UI; its catalog is complete by construction. */
export const defaultLocale = "en";

function canonical(tag: string): Intl.Locale | undefined {
  try {
    return new Intl.Locale(tag);
  } catch {
    return undefined;
  }
}

/**
 * Picks the best available locale for a list of requested ones, most
 * preferred first (the user's choice, then the browser languages, which are
 * what the browser sends as `Accept-Language`).
 *
 * For each requested locale, in order: an exact match ("pt-BR"), then the
 * available locale with the same language and script ("pt-BR" for "pt-PT"
 * when there is no "pt-PT", preferring the bare "pt"). Falls back to
 * `fallback`.
 */
export function negotiateLocale(
  requested: readonly string[],
  available: readonly string[],
  fallback: string = defaultLocale,
): string {
  const offered = available
    .map((tag) => ({ tag, locale: canonical(tag) }))
    .filter((o): o is { tag: string; locale: Intl.Locale } => o.locale !== undefined);
  for (const tag of requested) {
    const want = canonical(tag);
    if (!want) {
      continue;
    }
    const exact = offered.find((o) => o.locale.baseName === want.baseName);
    if (exact) {
      return exact.tag;
    }
    const script = want.maximize().script;
    const sameLanguage = offered.filter(
      (o) => o.locale.language === want.language && o.locale.maximize().script === script,
    );
    const best = sameLanguage.find((o) => !o.locale.region) ?? sameLanguage[0];
    if (best) {
      return best.tag;
    }
  }
  return fallback;
}

/** Parses an `Accept-Language` header into tags ordered by quality. */
export function parseAcceptLanguage(header: string): string[] {
  return header
    .split(",")
    .map((part, index) => {
      const [tag = "", ...params] = part.trim().split(";");
      const q = params.map((p) => p.trim()).find((p) => p.startsWith("q="));
      const quality = q ? Number.parseFloat(q.slice(2)) : 1;
      return { tag: tag.trim(), quality: Number.isFinite(quality) ? quality : 0, index };
    })
    .filter((e) => e.tag !== "" && e.tag !== "*" && e.quality > 0)
    .sort((a, b) => b.quality - a.quality || a.index - b.index)
    .map((e) => e.tag);
}
