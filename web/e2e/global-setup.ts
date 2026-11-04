import { existsSync } from "node:fs";

import { repo, run, saveState, work } from "./env";

/**
 * Builds the binary under test (web UI embedded) and starts one PostgreSQL
 * container; every test gets its own database and server (fixtures.ts).
 * ORGLANE_E2E_BINARY skips the build.
 */
export default async function globalSetup(): Promise<void> {
  let binary = process.env.ORGLANE_E2E_BINARY;
  if (!binary) {
    if (!existsSync(`${repo}web/dist/index.html`)) {
      run("pnpm", ["--silent", "--filter", "@orglane/web", "build"]);
    }
    binary = `${work}orglane`;
    run("go", ["build", "-tags", "githubimporttest", "-o", binary, "./core/cmd/orglane"]);
  }

  const container = run("docker", [
    "run",
    "-d",
    "--rm",
    "-e",
    "POSTGRES_USER=orglane",
    "-e",
    "POSTGRES_PASSWORD=orglane",
    "-e",
    "POSTGRES_DB=orglane",
    "-p",
    "127.0.0.1::5432",
    "postgres:18-alpine",
  ]);
  const port = run("docker", ["port", container, "5432/tcp"]).split("\n")[0]?.split(":").pop();
  for (let i = 0; ; i++) {
    try {
      run("docker", [
        "exec",
        container,
        "pg_isready",
        "-U",
        "orglane",
        "-d",
        "orglane",
        "-h",
        "127.0.0.1",
      ]);
      break;
    } catch (err) {
      if (i > 60) {
        run("docker", ["rm", "-f", container]);
        throw err;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
  }
  saveState({ container, postgres: `postgres://orglane:orglane@127.0.0.1:${port}`, binary });
}
