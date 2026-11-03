import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

/** The repository root, the web package and the e2e work directory. */
export const repo = fileURLToPath(new URL("../..", import.meta.url));
export const work = fileURLToPath(new URL("../test-results/.e2e/", import.meta.url));
const stateFile = `${work}state.json`;

export interface State {
  /** The PostgreSQL container started by the global setup. */
  container: string;
  /** postgres://... of its server, without a database name. */
  postgres: string;
  /** The orglane binary under test (with the web UI embedded). */
  binary: string;
}

export function saveState(state: State): void {
  mkdirSync(work, { recursive: true });
  writeFileSync(stateFile, JSON.stringify(state));
}

export function loadState(): State {
  return JSON.parse(readFileSync(stateFile, "utf8")) as State;
}

export function hasState(): boolean {
  return existsSync(stateFile);
}

export function run(
  cmd: string,
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): string {
  return execFileSync(cmd, args, {
    cwd: options.cwd ?? repo,
    env: options.env ?? process.env,
    encoding: "utf8",
  }).trim();
}
