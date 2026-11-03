import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { sha256Hex } from "../src/lib/installer.ts";
import { checkDist, externalResources, srcsetUrls } from "./dist-checks.ts";

const script = "#!/bin/sh\necho installer ✓\n";

let dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
  dirs = [];
});

function fixture(files: Record<string, string>) {
  const root = mkdtempSync(join(tmpdir(), "orglane-website-"));
  dirs.push(root);
  const installerSource = join(root, "install.sh");
  writeFileSync(installerSource, script);
  const distDir = join(root, "dist");
  for (const [name, content] of Object.entries(files)) {
    const path = join(distDir, name);
    mkdirSync(join(path, ".."), { recursive: true });
    writeFileSync(path, content);
  }
  return { distDir, installerSource, host: "orglane.app" };
}

const good = {
  "install.sh": script,
  "index.html": `<html><body><code>${sha256Hex(script)}</code><a href="https://github.com/orglane/orglane">GitHub</a></body></html>`,
  "404.html": "<html></html>",
  CNAME: "orglane.app\n",
};

describe("checkDist", () => {
  it("accepts a correct build", () => {
    expect(checkDist(fixture(good))).toEqual([]);
  });

  it("finds a missing or different installer", () => {
    const { "install.sh": _, ...withoutInstaller } = good;
    expect(checkDist(fixture(withoutInstaller))).toContain("install.sh is missing from the build");
    expect(checkDist(fixture({ ...good, "install.sh": `${script}# changed\n` }))).toContain(
      "install.sh in the build differs from deploy/install.sh",
    );
  });

  it("finds a page without the installer's checksum", () => {
    const problems = checkDist(fixture({ ...good, "index.html": "<html></html>" }));
    expect(problems).toEqual([
      `index.html does not show the installer's SHA-256 (${sha256Hex(script)})`,
    ]);
  });

  it("finds a missing 404 page and a wrong CNAME", () => {
    const { "404.html": _, ...without404 } = good;
    expect(checkDist(fixture({ ...without404, CNAME: "example.com" }))).toEqual([
      "404.html is missing from the build",
      'CNAME is "example.com", want "orglane.app"',
    ]);
  });

  it("finds resources from other origins in nested files", () => {
    const problems = checkDist(
      fixture({ ...good, "_astro/page.css": "body{background:url(https://cdn.example/x.png)}" }),
    );
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/^_astro\/page\.css loads from another origin/);
  });
});

describe("srcsetUrls", () => {
  it.each([
    ["/a.png", ["/a.png"]],
    ["/a.png 1x, /b.png 2x", ["/a.png", "/b.png"]],
    ["/a.png 480w,https://cdn.example/b.png 960w", ["/a.png", "https://cdn.example/b.png"]],
    ["/a.png,, /b.png", ["/a.png", "/b.png"]],
    [
      "data:image/png;base64,AAAA 1x, //cdn.example/b.png 2x",
      ["data:image/png;base64,AAAA", "//cdn.example/b.png"],
    ],
    ["  ", []],
  ])("parses %j", (value, urls) => {
    expect(srcsetUrls(value)).toEqual(urls);
  });
});

describe("externalResources", () => {
  it.each([
    '<script src="https://cdn.example/a.js"></script>',
    "<img src=//cdn.example/a.png>",
    '<img srcset="https://cdn.example/a.png 1x">',
    // Regression (code review): an external candidate after a local one.
    '<img src="/a.png" srcset="/a.png 1x, https://cdn.example/a-2x.png 2x">',
    "<source srcset='/a.webp 480w,//cdn.example/b.webp 960w'>",
    '<link rel="preload" as="image" imagesrcset="/a.png 1x, https://cdn.example/b.png 2x">',
    '<link rel="stylesheet" href="https://fonts.example/css">',
    '<link rel="preconnect" href="https://fonts.example">',
    '<link rel="icon" href="https://cdn.example/icon.svg">',
    "@import url('https://fonts.example/css');",
    'div{background:url("//cdn.example/a.png")}',
  ])("finds %s", (content) => {
    expect(externalResources(content).length).toBeGreaterThan(0);
  });

  it.each([
    '<a href="https://github.com/orglane/orglane">GitHub</a>',
    '<link rel="canonical" href="https://orglane.app/">',
    '<meta property="og:url" content="https://orglane.app/">',
    '<script src="/_astro/page.js"></script>',
    '<link rel="icon" href="/favicon.svg">',
    "div{background:url(/a.png)}",
    '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
    '<img srcset="/a.png 1x, /a-2x.png 2x">',
    '<img srcset="data:image/png;base64,iVBORw0KGgo= 1x, /a-2x.png 2x">',
  ])("allows %s", (content) => {
    expect(externalResources(content)).toEqual([]);
  });
});
