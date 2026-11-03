import { test } from "@playwright/test";

// Screenshots for UI review:
//   ORGLANE_SCREENSHOTS=1 make test-website
// writes .github/pr-assets/website/website-<viewport>-<theme>.png. Skipped otherwise.
test.skip(!process.env.ORGLANE_SCREENSHOTS, "set ORGLANE_SCREENSHOTS=1 to capture screenshots");

const viewports = {
  desktop: { width: 1280, height: 800 },
  mobile: { width: 360, height: 780 },
} as const;

for (const [viewport, size] of Object.entries(viewports)) {
  for (const colorScheme of ["light", "dark"] as const) {
    test(`website ${viewport} ${colorScheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme });
      await page.setViewportSize(size);
      await page.goto("/");
      await page.screenshot({
        path: new URL(
          `../../.github/pr-assets/website/website-${viewport}-${colorScheme}.png`,
          import.meta.url,
        ).pathname,
        fullPage: true,
      });
    });
  }
}
