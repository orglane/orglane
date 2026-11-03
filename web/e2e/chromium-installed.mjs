// Exits 0 when Playwright's Chromium is installed (used by `make test`).
import { existsSync } from "node:fs";

import { chromium } from "@playwright/test";

process.exit(existsSync(chromium.executablePath()) ? 0 : 1);
