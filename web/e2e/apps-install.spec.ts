import { repo } from "./env";
import { expect, test } from "./fixtures";

// Managed services on with a real agent process but no Compose bundle: the
// install goes through the API, the agent accepts it and fails once it
// needs the bundle's PostgreSQL. The agent waits up to two minutes for that
// container to appear (#101), so the failure expectation is generous. The
// success path is covered by the Compose acceptance test (tests/compose
// TestManagedProfile).
test.use({ demo: true, realAgent: true });

test("an Admin installs Forgejo from Settings → Apps and sees it installing, then failed", async ({
  page,
  instance,
}) => {
  test.setTimeout(210_000);
  const signedIn = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  expect(signedIn.ok()).toBeTruthy();
  await page.goto(`${instance.url}/settings/apps`);
  await page.getByRole("link", { name: "Install Forgejo" }).click();
  await expect(page.getByRole("heading", { name: "Install Forgejo", level: 1 })).toBeVisible();

  // The agent answers: no enable-managed command; the defaults are filled in.
  const form = page.getByRole("form", { name: "Install Forgejo" });
  await expect(form.getByLabel("Address")).toHaveValue("git.orglane.localhost");
  await expect(form.getByLabel("Organization")).toHaveValue("orglane");
  await expect(page.getByText("sh -s -- enable-managed")).toHaveCount(0);
  await expect(page.getByText(/Needs at least 256 MB of memory/)).toBeVisible();

  // Invalid input stays in the form.
  await form.getByLabel("Key").fill("Git!");
  await form.getByRole("button", { name: "Install" }).click();
  await expect(form.getByLabel("Key")).toBeFocused();
  await expect(form.getByText(/lowercase letters, digits and hyphens/)).toBeVisible();
  await form.getByLabel("Key").fill("git");
  // A long name the form accepts must wrap wherever it shows.
  const longName = `Forgejo${"x".repeat(93)}`;
  await form.getByLabel("Name").fill(longName);
  await form.getByRole("button", { name: "Install" }).click();

  // The instance page: installing, then the agent's operation fails without
  // the bundle and the instance fails with an explanation.
  await expect(page.getByRole("heading", { name: longName, exact: true, level: 1 })).toBeVisible({
    timeout: 30_000,
  });
  const operations = page.getByRole("table", { name: "Operations" });
  await expect(operations.getByRole("row").filter({ hasText: "Install" })).toBeVisible();
  await expect(page.getByText(/Installing this app failed/)).toBeVisible({
    timeout: 150_000,
  });
  const installRow = operations.getByRole("row").filter({ hasText: "Install" });
  await expect(installRow.getByText("Failed", { exact: true })).toBeVisible();
  // Without the bundle the agent finds no PostgreSQL for the app's database.
  await expect(installRow.getByText("database_failed")).toBeVisible();

  // A failed app can be removed: the confirmation says what it does.
  await page.getByRole("button", { name: "Remove", exact: true }).click();
  const panel = page.getByRole("group", { name: "Remove" });
  await expect(panel.getByText(/data, database and settings are kept/)).toBeVisible();
  await expect(panel.getByRole("button", { name: "Cancel" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "Remove", exact: true })).toBeFocused();

  // No horizontal overflow at 360 px, on the instance page and in the list.
  await page.setViewportSize({ width: 360, height: 780 });
  const overflow = () =>
    page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
  expect(await overflow()).toBeLessThanOrEqual(0);
  await page.goto(`${instance.url}/settings/apps`);
  await expect(page.getByRole("link", { name: longName })).toBeVisible();
  expect(await overflow()).toBeLessThanOrEqual(0);
  await page.setViewportSize({ width: 1280, height: 720 });

  if (process.env.ORGLANE_SCREENSHOTS) {
    for (const [name, size] of [
      ["desktop", { width: 1280, height: 900 }],
      ["mobile", { width: 360, height: 780 }],
    ] as const) {
      await page.setViewportSize(size);
      for (const theme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
        await page.screenshot({
          path: `${repo}.github/pr-assets/apps/app-instance-failed-${name}-${theme}.png`,
          fullPage: true,
        });
      }
    }
    await page.goto(`${instance.url}/settings/apps/install/forgejo`);
    await expect(page.getByRole("form", { name: "Install Forgejo" })).toBeVisible();
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      await page.screenshot({
        path: `${repo}.github/pr-assets/apps/install-forgejo-mobile-${theme}.png`,
        fullPage: true,
      });
    }
  }
});
