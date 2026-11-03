// Verifies the built site (run by `pnpm build` after `astro build`): the
// installer is byte-identical to deploy/install.sh, the page shows its
// SHA-256, CNAME matches the site's host, and nothing loads from other
// origins.

import { fileURLToPath } from "node:url";
import { siteHost } from "../src/lib/site.ts";
import { checkDist } from "./dist-checks.ts";

const problems = checkDist({
  distDir: fileURLToPath(new URL("../dist", import.meta.url)),
  installerSource: fileURLToPath(new URL("../../deploy/install.sh", import.meta.url)),
  host: siteHost,
});

if (problems.length > 0) {
  for (const problem of problems) console.error(`error: ${problem}`);
  process.exit(1);
}
console.log(
  "dist: install.sh matches deploy/install.sh; checksum, CNAME and same-origin resources OK",
);
