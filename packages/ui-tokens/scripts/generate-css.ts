// Writes tokens.css from the token sources; with --check, fails when the
// committed file is stale instead (part of `make lint`).
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { toCss } from "../src/css.ts";

const out = fileURLToPath(new URL("../tokens.css", import.meta.url));
const css = toCss();
if (process.argv.includes("--check")) {
  let current = "";
  try {
    current = readFileSync(out, "utf8");
  } catch {}
  if (current !== css) {
    console.error("error: packages/ui-tokens/tokens.css is stale, run 'make generate'");
    process.exit(1);
  }
} else {
  writeFileSync(out, css);
  console.log("wrote packages/ui-tokens/tokens.css");
}
