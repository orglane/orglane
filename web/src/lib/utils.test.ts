import { describe, expect, it } from "vitest";

import { initials, safeRedirect, welcomeDestination } from "./utils";

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

describe("welcomeDestination", () => {
  it.each([
    ["/teams", "/teams"],
    ["/welcome?redirect=%2Fteams", "/teams"],
    ["/welcome?redirect=%2Fteams%3Fview%3Dall%23people", "/teams?view=all#people"],
    ["/welcome?redirect=%2Fwelcome%3Fredirect%3D%252Fteams", "/teams"],
    ["/welcome/passkey?redirect=%2Fwelcome%3Fredirect%3D%252Fteams", "/teams"],
    ["/welcome-news", "/welcome-news"],
    ["/welcome", "/"],
    ["/welcome?redirect=%2Fwelcome", "/"],
    ["https://evil.example", "/"],
    ["/welcome?redirect=https%3A%2F%2Fevil.example", "/"],
    ["/welcome?redirect=%2F%2Fevil.example", "/"],
    ["/welcome?redirect=%2F%5Cevil.example", "/"],
  ])("normalizes %s to %s", (target, expected) => {
    expect(welcomeDestination(target)).toBe(expected);
  });

  it("bounds nested welcome continuations", () => {
    let nested = "/teams";
    for (let depth = 0; depth < 6; depth++) {
      nested = `/welcome?redirect=${encodeURIComponent(nested)}`;
    }
    expect(welcomeDestination(nested)).toBe("/");
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
