// The installer served at /install.sh. deploy/install.sh is the single source
// of truth: it is read at build time (Vite `?raw` import), served as-is, and
// its SHA-256 is shown on the landing page. scripts/check-dist.ts verifies
// that the built file is byte-identical to the source.

import { createHash } from "node:crypto";
import script from "../../../deploy/install.sh?raw";

export const installerScript: string = script;

export function sha256Hex(data: string | Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

export const installerSha256: string = sha256Hex(installerScript);
