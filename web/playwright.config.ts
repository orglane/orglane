import { defineConfig, devices } from "@playwright/test";

/**
 * End-to-end tests of the web UI against a real `orglane server` and
 * PostgreSQL (Docker); see e2e/README.md. `make test-e2e` runs them, and
 * `make test` does too when Docker and Playwright's Chromium are available.
 */
export default defineConfig({
  testDir: "./e2e",
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], channel: "chromium" } }],
});
