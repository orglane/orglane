// i18n catalog tooling for the web app (FormatJS, ICU MessageFormat).
//
//   node scripts/i18n.ts extract   collect messages from src/ into src/i18n/locales/en.json
//   node scripts/i18n.ts check     fail when en.json is stale or another catalog is inconsistent
//
// Every message is declared in code with an explicit, namespaced id and its
// English text. Other catalogs (src/i18n/locales/<tag>.json) may lag behind
// English (missing ids fall back to English at runtime) but must not contain
// unknown ids, invalid ICU syntax or different placeholders.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { type MessageFormatElement, parse, TYPE } from "@formatjs/icu-messageformat-parser";

const root = fileURLToPath(new URL("..", import.meta.url));
const localesDir = join(root, "src/i18n/locales");
const sourceCatalog = join(localesDir, "en.json");
const idPattern = /^[a-z][A-Za-z0-9]*(?:\.[a-z][A-Za-z0-9]*)+$/;

type Catalog = Record<string, string>;

function extract(): Catalog {
  const dir = mkdtempSync(join(tmpdir(), "orglane-i18n-"));
  const out = join(dir, "en.json");
  try {
    execFileSync(
      "formatjs",
      [
        "extract",
        "src/**/*.{ts,tsx}",
        "--ignore",
        "src/**/*.test.{ts,tsx}",
        "--format",
        "simple",
        "--out-file",
        out,
        "--throws",
      ],
      { cwd: root, stdio: ["ignore", "inherit", "inherit"] },
    );
    const catalog = JSON.parse(readFileSync(out, "utf8")) as Catalog;
    return Object.fromEntries(Object.entries(catalog).sort(([a], [b]) => a.localeCompare(b)));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

function serialize(catalog: Catalog): string {
  return `${JSON.stringify(catalog, null, 2)}\n`;
}

/** The arguments and tags a message uses, e.g. ["arg:name", "tag:code"]. */
export function placeholders(message: string): string[] {
  const found = new Set<string>();
  const walk = (elements: MessageFormatElement[]) => {
    for (const el of elements) {
      switch (el.type) {
        case TYPE.argument:
        case TYPE.number:
        case TYPE.date:
        case TYPE.time:
          found.add(`arg:${el.value}`);
          break;
        case TYPE.select:
        case TYPE.plural:
          found.add(`arg:${el.value}`);
          for (const option of Object.values(el.options)) {
            walk(option.value);
          }
          break;
        case TYPE.tag:
          found.add(`tag:${el.value}`);
          walk(el.children);
          break;
        default:
          break;
      }
    }
  };
  walk(parse(message));
  return [...found].sort();
}

/** Problems of one catalog compared with the English source. */
export function lint(locale: string, catalog: Catalog, source: Catalog): string[] {
  const problems: string[] = [];
  for (const [id, message] of Object.entries(catalog)) {
    if (!(id in source)) {
      problems.push(`${locale}: unknown message id ${id}`);
      continue;
    }
    let mine: string[];
    try {
      mine = placeholders(message);
    } catch (err) {
      problems.push(`${locale}: ${id}: invalid ICU message: ${(err as Error).message}`);
      continue;
    }
    const want = placeholders(source[id] ?? "");
    if (mine.join() !== want.join()) {
      problems.push(
        `${locale}: ${id}: placeholders ${mine.join(", ") || "(none)"} differ from English (${want.join(", ") || "none"})`,
      );
    }
  }
  return problems;
}

function main(command: string | undefined): number {
  const extracted = extract();
  const badIds = Object.keys(extracted).filter((id) => !idPattern.test(id));
  if (badIds.length > 0) {
    console.error(
      `error: messages need an explicit, namespaced id ("area.name"): ${badIds.join(", ")}`,
    );
    return 1;
  }
  if (command === "extract") {
    writeFileSync(sourceCatalog, serialize(extracted));
    console.log(`wrote ${Object.keys(extracted).length} messages to src/i18n/locales/en.json`);
    return 0;
  }
  if (command !== "check") {
    console.error("usage: node scripts/i18n.ts extract|check");
    return 2;
  }
  const problems: string[] = [];
  if (readFileSync(sourceCatalog, "utf8") !== serialize(extracted)) {
    problems.push(
      "en: src/i18n/locales/en.json is stale, run 'pnpm --filter @orglane/web i18n:extract'",
    );
  }
  for (const file of readdirSync(localesDir).filter(
    (f) => f.endsWith(".json") && f !== "en.json",
  )) {
    const locale = file.replace(/\.json$/, "");
    problems.push(
      ...lint(
        locale,
        JSON.parse(readFileSync(join(localesDir, file), "utf8")) as Catalog,
        extracted,
      ),
    );
  }
  for (const p of problems) {
    console.error(`error: ${p}`);
  }
  return problems.length > 0 ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exit(main(process.argv[2]));
}
