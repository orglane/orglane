import { describe, expect, it } from "vitest";

import { cssVar } from "./index.js";

describe("cssVar", () => {
  it("builds a prefixed CSS variable reference", () => {
    expect(cssVar("radius-md")).toBe("var(--orglane-radius-md)");
  });

  it("rejects names that are not kebab-case", () => {
    expect(() => cssVar("Radius MD")).toThrow(/invalid token name/);
    expect(() => cssVar("")).toThrow(/invalid token name/);
  });
});
