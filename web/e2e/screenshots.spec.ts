import type { Page } from "@playwright/test";
import { repo } from "./env";
import { expect, owner, setUp, signOut, test } from "./fixtures";

// Captures every screen in light and dark for UI review:
//   ORGLANE_SCREENSHOTS=1 pnpm --filter @orglane/web test:e2e screenshots
// writes .github/pr-assets/web/<screen>-<theme>.png. Skipped otherwise.
test.skip(!process.env.ORGLANE_SCREENSHOTS, "set ORGLANE_SCREENSHOTS=1 to capture screenshots");

const themes = ["light", "dark"] as const;

async function shoot(page: Page, name: string) {
  for (const theme of themes) {
    await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${theme}\\b`));
    await page.screenshot({
      path: `${repo}.github/pr-assets/web/${name}-${theme}.png`,
      animations: "disabled",
    });
  }
}

test("screenshots", async ({ page, instance, playwright, webauthn: _ }) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ width: 1280, height: 820 });

  await page.goto(instance.url);
  await expect(page.getByRole("heading", { name: "Set up your workspace" })).toBeVisible();
  await shoot(page, "setup");

  await setUp(page, instance);
  await expect(page.getByRole("heading", { name: "Sign in faster with a passkey" })).toBeVisible();
  await shoot(page, "passkey-offer");
  await page.getByRole("button", { name: "Skip for now" }).click();

  await expect(page.getByRole("heading", { level: 1 })).toContainText("Alex");
  await shoot(page, "home");
  for (const section of ["inbox", "people", "apps"]) {
    await page.goto(`${instance.url}/${section}`);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await shoot(page, section);
  }

  // A second session from the same browser, as after signing in elsewhere.
  const other = await playwright.request.newContext({
    baseURL: instance.url,
    userAgent: await page.evaluate(() => navigator.userAgent),
    extraHTTPHeaders: { Origin: instance.url },
  });
  expect(
    (
      await other.post("/api/v1/auth/sign-in", {
        data: { email: owner.email, password: owner.password },
      })
    ).ok(),
  ).toBe(true);
  await page.goto(`${instance.url}/settings/security`);
  await expect(page.getByText("This device")).toBeVisible();
  await expect(page.getByRole("button", { name: /^Sign out of / })).toHaveCount(2);
  await shoot(page, "security");

  await page.getByRole("button", { name: `Account menu for ${owner.name}` }).click();
  await page.getByRole("menuitem", { name: "Theme" }).hover();
  await expect(page.getByRole("menuitemradio", { name: "System" })).toBeVisible();
  await shoot(page, "profile-menu");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");

  await signOut(page);
  await shoot(page, "sign-in");

  await page.getByLabel("Work email").fill(owner.email);
  await page.getByLabel("Password", { exact: true }).fill("not the password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await shoot(page, "sign-in-error");

  // Phones (360px is the narrowest supported width).
  await page.setViewportSize({ width: 360, height: 780 });
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Alex");
  await shoot(page, "mobile-home");
  await page.goto(`${instance.url}/settings/security`);
  await expect(page.getByText("This device")).toBeVisible();
  await shoot(page, "mobile-security");
});
