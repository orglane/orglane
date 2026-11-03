// Checks of the built site (dist/), run after every build by check-dist.ts
// and unit-tested in dist-checks.test.ts.

import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";

export interface DistCheckInput {
  /** The built site. */
  distDir: string;
  /** deploy/install.sh, the installer's single source. */
  installerSource: string;
  /** The host the site is served on (src/lib/site.ts). */
  host: string;
}

function files(dir: string): string[] {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => join(entry.parentPath, entry.name));
}

function read(path: string): Buffer | undefined {
  try {
    return readFileSync(path);
  } catch {
    return undefined;
  }
}

// Resource references that make a browser fetch from another origin:
// src/poster/data attributes, every candidate of srcset/imagesrcset, <link>
// elements other than navigation (stylesheets, icons, preloads, manifests),
// CSS url() and @import. Plain navigation links (<a href>) to other sites are
// fine.
const externalUrl = "(?:https?:)?//";
const isExternal = (url: string) => new RegExp(`^${externalUrl}`, "i").test(url);
const resourcePatterns: RegExp[] = [
  new RegExp(String.raw`\s(?:src|poster|data)\s*=\s*["']?\s*${externalUrl}`, "i"),
  new RegExp(String.raw`url\(\s*["']?\s*${externalUrl}`, "i"),
  new RegExp(String.raw`@import\s+(?:url\()?\s*["']?\s*${externalUrl}`, "i"),
];
const srcsetAttribute = /\s(?:image)?srcset\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))/gi;

/**
 * The URLs of a srcset value, following the HTML candidate syntax: a URL up
 * to whitespace (trailing commas end the candidate), then optional
 * descriptors up to the next comma outside parentheses.
 */
export function srcsetUrls(value: string): string[] {
  const urls: string[] = [];
  let i = 0;
  while (i < value.length) {
    while (i < value.length && /[\s,]/.test(value.charAt(i))) i++;
    if (i >= value.length) break;
    const start = i;
    while (i < value.length && !/\s/.test(value.charAt(i))) i++;
    let url = value.slice(start, i);
    if (url.endsWith(",")) {
      url = url.replace(/,+$/, "");
    } else {
      let depth = 0;
      while (i < value.length && (value.charAt(i) !== "," || depth > 0)) {
        if (value.charAt(i) === "(") depth++;
        if (value.charAt(i) === ")") depth = Math.max(0, depth - 1);
        i++;
      }
    }
    if (url) urls.push(url);
  }
  return urls;
}

const linkElement = /<link\b[^>]*>/gi;
const navigationRels = new Set(["canonical", "alternate", "author", "license", "me"]);

/** External resources a file would load, as short descriptions. */
export function externalResources(content: string): string[] {
  const found: string[] = [];
  for (const pattern of resourcePatterns) {
    const match = content.match(pattern);
    if (match) found.push(match[0].trim());
  }
  for (const [attribute, ...values] of content.matchAll(srcsetAttribute)) {
    const value = values.find((v) => v !== undefined) ?? "";
    if (srcsetUrls(value).some(isExternal)) found.push(attribute.trim());
  }
  for (const [element] of content.matchAll(linkElement)) {
    const href = element.match(/\shref\s*=\s*["']?([^"'\s>]+)/i)?.[1] ?? "";
    const rels = (element.match(/\srel\s*=\s*["']?([^"'>]+)/i)?.[1] ?? "")
      .toLowerCase()
      .split(/\s+/);
    if (isExternal(href) && !rels.every((rel) => navigationRels.has(rel))) {
      found.push(element);
    }
  }
  return found;
}

/** Problems with the built site; empty when it is fine. */
export function checkDist({ distDir, installerSource, host }: DistCheckInput): string[] {
  const problems: string[] = [];
  const source = read(installerSource);
  if (!source) return [`cannot read the installer source ${installerSource}`];

  const served = read(join(distDir, "install.sh"));
  if (!served) {
    problems.push("install.sh is missing from the build");
  } else if (!served.equals(source)) {
    problems.push("install.sh in the build differs from deploy/install.sh");
  }

  const index = read(join(distDir, "index.html"))?.toString("utf8");
  if (!index) {
    problems.push("index.html is missing from the build");
  } else {
    const sha256 = createHash("sha256").update(source).digest("hex");
    if (!index.includes(sha256)) {
      problems.push(`index.html does not show the installer's SHA-256 (${sha256})`);
    }
  }

  if (!read(join(distDir, "404.html"))) problems.push("404.html is missing from the build");

  const cname = read(join(distDir, "CNAME"))?.toString("utf8").trim();
  if (cname !== host)
    problems.push(`CNAME is ${JSON.stringify(cname)}, want ${JSON.stringify(host)}`);

  for (const file of files(distDir)) {
    if (!/\.(html|css|js|mjs|svg|webmanifest)$/i.test(file)) continue;
    for (const resource of externalResources(readFileSync(file, "utf8"))) {
      problems.push(`${relative(distDir, file)} loads from another origin: ${resource}`);
    }
  }
  return problems;
}
