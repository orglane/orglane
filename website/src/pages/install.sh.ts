// /install.sh: the installer, written to dist/install.sh at build time. The
// static host picks the Content-Type from the extension (GitHub Pages:
// application/x-sh); the header here applies to `astro dev`.

import type { APIRoute } from "astro";
import { installerScript } from "../lib/installer.ts";

export const GET: APIRoute = () =>
  new Response(installerScript, {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
