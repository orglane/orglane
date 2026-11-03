import { describe, expect, it } from "vitest";

import { initials, safeRedirect } from "./utils";

describe("safeRedirect", () => {
  it("keeps paths on this origin", () => {
    expect(safeRedirect("/inbox")).toBe("/inbox");
    expect(safeRedirect("/people?team=eng#top")).toBe("/people?team=eng#top");
  });

  it("refuses anything that could leave the workspace", () => {
    for (const bad of [
      "https://evil.example/",
      "//evil.example",
      "/\\evil.example",
      "javascript:alert(1)",
      "inbox",
      "",
      undefined,
      42,
    ]) {
      expect(safeRedirect(bad)).toBe("/");
    }
    expect(safeRedirect("//evil.example", "/home")).toBe("/home");
  });
});

describe("initials", () => {
  it("uses the first and last name", () => {
    expect(initials("Alex Morgan")).toBe("AM");
    expect(initials("  ada  ")).toBe("A");
    expect(initials("Jean Luc Picard")).toBe("JP");
    expect(initials("")).toBe("?");
  });
});
