import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { installerScript, installerSha256, sha256Hex } from "./installer.ts";
import {
  absoluteUrl,
  installerUrl,
  repositoryFile,
  reservedRoutes,
  routes,
  siteHost,
} from "./site.ts";

describe("site", () => {
  it("serves the installer from orglane.app/install.sh", () => {
    expect(installerUrl).toBe("https://orglane.app/install.sh");
  });

  it("matches public/CNAME", () => {
    expect(readFileSync(new URL("../../public/CNAME", import.meta.url), "utf8").trim()).toBe(
      siteHost,
    );
  });

  it("builds absolute and repository URLs", () => {
    expect(absoluteUrl("/")).toBe("https://orglane.app/");
    expect(repositoryFile("ROADMAP.md")).toBe(
      "https://github.com/orglane/orglane/blob/main/ROADMAP.md",
    );
  });

  it("builds nothing on the paths ADR 0018 reserves", () => {
    const built = Object.values(routes) as string[];
    for (const reserved of reservedRoutes) {
      expect(built.some((route) => route === reserved || route.startsWith(`${reserved}/`))).toBe(
        false,
      );
    }
  });
});

describe("installer", () => {
  it("is deploy/install.sh", () => {
    const source = readFileSync(new URL("../../../deploy/install.sh", import.meta.url));
    expect(Buffer.from(installerScript, "utf8").equals(source)).toBe(true);
    expect(installerSha256).toBe(sha256Hex(source));
  });
});
