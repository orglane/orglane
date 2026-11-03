import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

const run = promisify(execFile);
const packageDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const generator = path.join(packageDir, "codegen/generate.mjs");
const spec = path.resolve(packageDir, "../../core/api/openapi.yaml");
const committed = path.join(packageDir, "src/generated/schema.ts");

function generate(...args: string[]) {
  return run(process.execPath, [generator, ...args]);
}

// `make lint` runs `generate.mjs --check`; these tests pin down that it catches drift.
describe("generated client", () => {
  let dir: string;
  let changedSpec: string;

  beforeAll(async () => {
    dir = await mkdtemp(path.join(tmpdir(), "orglane-api-client-"));
    // A contract change that has not been regenerated: one new operation.
    const operation = [
      "  /api/v1/ping:",
      "    get:",
      "      operationId: getPing",
      "      x-orglane-permission: public",
      "      responses:",
      '        "204":',
      "          description: Pong.",
      "",
    ].join("\n");
    changedSpec = path.join(dir, "openapi.yaml");
    const original = await readFile(spec, "utf8");
    expect(original).toContain("\npaths:\n");
    await writeFile(changedSpec, original.replace("\npaths:\n", `\npaths:\n${operation}`));
  });

  afterAll(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it("is up to date with core/api/openapi.yaml", async () => {
    await expect(generate("--check")).resolves.toBeDefined();
  });

  it("is reported stale when the contract changes without regenerating", async () => {
    const result = generate("--check", "--spec", changedSpec);
    await expect(result).rejects.toMatchObject({
      code: 1,
      stderr: expect.stringContaining("is stale, run 'make generate'"),
    });
  });

  it("is reported stale when missing", async () => {
    const missing = path.join(dir, "missing.ts");
    await expect(generate("--check", "--out", missing)).rejects.toMatchObject({ code: 1 });
  });

  it("picks up the change once regenerated", async () => {
    const out = path.join(dir, "schema.ts");
    await generate("--spec", changedSpec, "--out", out);

    expect(await readFile(out, "utf8")).toContain('"/api/v1/ping"');
    await expect(generate("--check", "--spec", changedSpec, "--out", out)).resolves.toBeDefined();
    expect(await readFile(committed, "utf8")).not.toContain('"/api/v1/ping"');
  });
});
