import { defineConfig } from "vitest/config";

// Unit tests only; the Playwright tests in e2e/ run with `test:e2e`.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts", "scripts/**/*.test.ts"],
  },
});
