// @vitest-environment node
import { describe, expect, it } from "vitest";

import { lint, placeholders } from "../../scripts/i18n";
import en from "./locales/en.json";

describe("the English catalog", () => {
  it("has namespaced ids and valid ICU messages", () => {
    for (const [id, message] of Object.entries(en)) {
      expect(id).toMatch(/^[a-z][A-Za-z0-9]*(\.[a-z][A-Za-z0-9]*)+$/);
      expect(() => placeholders(message)).not.toThrow();
    }
  });
});

describe("catalog lint", () => {
  const source = {
    "a.greeting": "Hello {name}",
    "a.plain": "Plain",
    "a.rich": "Run <code>x</code>",
  };

  it("accepts a consistent translation, even when incomplete", () => {
    expect(lint("de", { "a.greeting": "Hallo {name}" }, source)).toEqual([]);
  });

  it("reports unknown ids, bad syntax and changed placeholders", () => {
    const problems = lint(
      "de",
      {
        "a.nope": "x",
        "a.greeting": "Hallo {nom}",
        "a.plain": "Kaputt {",
        "a.rich": "Führe x aus",
      },
      source,
    );
    expect(problems).toHaveLength(4);
    expect(problems.join("\n")).toMatch(/unknown message id a\.nope/);
    expect(problems.join("\n")).toMatch(/a\.greeting: placeholders arg:nom/);
    expect(problems.join("\n")).toMatch(/a\.plain: invalid ICU message/);
    expect(problems.join("\n")).toMatch(/a\.rich: placeholders \(none\)/);
  });
});
