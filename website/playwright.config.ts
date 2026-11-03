import { defineConfig, devices } from "@playwright/test";

/**
 * End-to-end tests of the built site (dist/), served by `astro preview`.
 * `make test-website` builds and runs them; `make test` does too when
 * Playwright's Chromium is installed.
 */
const port = 4329;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: "list",
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
  },
  webServer: {
    // --ignore-lock keeps the server in the foreground (Astro backgrounds it
    // when it detects a coding agent) and away from a developer's preview.
    command: `pnpm --silent run preview --ignore-lock --host 127.0.0.1 --port ${port}`,
    url: `http://127.0.0.1:${port}/`,
    reuseExistingServer: false,
    timeout: 30_000,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], channel: "chromium" } }],
});
