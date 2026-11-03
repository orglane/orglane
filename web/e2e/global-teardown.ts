import { hasState, loadState, run } from "./env";

export default async function globalTeardown(): Promise<void> {
  if (hasState()) {
    run("docker", ["rm", "-f", loadState().container]);
  }
}
