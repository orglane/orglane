// Generates the TypeScript types of @orglane/api-client from the OpenAPI
// contract with openapi-typescript.
//
//   node generate.mjs           write packages/api-client/src/generated/schema.ts
//   node generate.mjs --check   exit with status 1 if that file is stale
//
// --spec <path> and --out <path> override the input and output files.
// `make generate` and `make lint` call this through the package scripts.
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parseArgs } from "node:util";

import openapiTS, { astToString } from "openapi-typescript";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "../../..");

const HEADER = `/**
 * Generated from core/api/openapi.yaml by openapi-typescript.
 * Do not edit: change the contract and run \`make generate\`.
 */

`;

const { values: flags } = parseArgs({
  options: {
    check: { type: "boolean", default: false },
    spec: { type: "string", default: path.join(repoRoot, "core/api/openapi.yaml") },
    out: { type: "string", default: path.resolve(here, "../src/generated/schema.ts") },
  },
});

const specPath = path.resolve(flags.spec);
const outPath = path.resolve(flags.out);
const outName = path.relative(repoRoot, outPath);

const ast = await openapiTS(pathToFileURL(specPath), { silent: true });
const generated = `${HEADER}${astToString(ast)}`;

if (flags.check) {
  const current = await readFile(outPath, "utf8").catch(() => null);
  if (current !== generated) {
    console.error(`error: ${outName} is stale, run 'make generate'`);
    process.exit(1);
  }
} else {
  await mkdir(path.dirname(outPath), { recursive: true });
  await writeFile(outPath, generated);
}
