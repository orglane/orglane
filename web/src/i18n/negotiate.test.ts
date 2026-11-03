import { describe, expect, it } from "vitest";

import { negotiateLocale, parseAcceptLanguage } from "./negotiate";

describe("negotiateLocale", () => {
  const available = ["de", "en", "pt-BR", "zh-Hant"];

  it("prefers an exact match", () => {
    expect(negotiateLocale(["pt-BR"], available)).toBe("pt-BR");
    expect(negotiateLocale(["de-DE", "en"], ["de-DE", "de", "en"])).toBe("de-DE");
  });

  it("falls back to the same language", () => {
    expect(negotiateLocale(["de-AT"], available)).toBe("de");
    expect(negotiateLocale(["pt-PT"], available)).toBe("pt-BR");
  });

  it("respects the script", () => {
    expect(negotiateLocale(["zh-TW"], available)).toBe("zh-Hant");
    expect(negotiateLocale(["zh-CN"], available)).toBe("en");
  });

  it("walks the preference list in order", () => {
    expect(negotiateLocale(["fr-FR", "de"], available)).toBe("de");
  });

  it("ignores invalid tags and falls back to English", () => {
    expect(negotiateLocale(["not a tag!", ""], available)).toBe("en");
    expect(negotiateLocale([], available)).toBe("en");
    expect(negotiateLocale(["ru"], ["en"])).toBe("en");
  });
});

describe("parseAcceptLanguage", () => {
  it("orders tags by quality, then position", () => {
    expect(parseAcceptLanguage("fr-CH, fr;q=0.9, en;q=0.8, de;q=0.7, *;q=0.5")).toEqual([
      "fr-CH",
      "fr",
      "en",
      "de",
    ]);
    expect(parseAcceptLanguage("en;q=0.5, ru")).toEqual(["ru", "en"]);
  });

  it("drops refused and empty entries", () => {
    expect(parseAcceptLanguage("de;q=0, , en")).toEqual(["en"]);
    expect(parseAcceptLanguage("")).toEqual([]);
  });
});
