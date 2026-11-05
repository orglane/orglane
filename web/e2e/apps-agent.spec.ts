import type { Page } from "@playwright/test";
import { repo } from "./env";
import { expect, test } from "./fixtures";

// Managed services are on, but no agent runs: the core's agent_sync and
// agent_apply jobs find nothing at the socket and keep the install waiting.
test.use({ demo: true, agent: true });

// The managed instance, desired spec generation 1 and its queued operation,
// as apps.Service.Install records them (the install API is #61). The test
// build registers the managed test provider for the agenttest package.
const install = (port: string) => `WITH i AS (
    INSERT INTO app_instances (key, provider, management, display_name, state, base_url, settings, access_policy, package_id, desired_generation)
    VALUES ('managed-test', 'agenttest', 'managed', 'Managed', 'installing', 'http://agenttest.localhost:${port}', '{}', 'all_people', 'agenttest', 1)
    RETURNING id),
  d AS (INSERT INTO app_desired_specs (instance_id, generation, package_version, run_state, settings, credential_versions, reason)
    SELECT id, 1, '1.0.0-1', 'running', '{}', '{}', 'install' FROM i),
  o AS (INSERT INTO app_operations (instance_id, generation, kind, state, to_version)
    SELECT id, 1, 'apply', 'queued', '1.0.0-1' FROM i)
  SELECT id FROM i`;

async function signIn(page: Page, url: string) {
  const signedIn = await page.request.post(`${url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: url },
  });
  expect(signedIn.ok()).toBeTruthy();
}

test("Settings → Apps shows a managed install waiting for the agent and its operation history", async ({
  page,
  instance,
}) => {
  test.setTimeout(90_000);
  const port = new URL(instance.url).port;
  const instanceId = instance.sql(install(port)).split("\n")[0] ?? "";
  await signIn(page, instance.url);

  // The sync (every 10 s here) queues the push; the push finds no agent.
  const row = page
    .getByRole("table", { name: "Connected apps" })
    .getByRole("row")
    .filter({ hasText: "Managed" });
  await expect(async () => {
    await page.goto(`${instance.url}/settings/apps`);
    await expect(row.getByText("Waiting for the agent")).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 45_000 });
  await expect(row.getByText("Installing")).toBeVisible();

  await row.getByRole("link", { name: "Managed" }).click();
  await expect(page.getByRole("heading", { name: "Managed", level: 1 })).toBeVisible();
  await expect(page.getByText("To start the agent, run this on the server")).toBeVisible();
  await expect(page.getByText("sh -s -- enable-managed")).toBeVisible();
  const operations = page.getByRole("table", { name: "Operations" });
  const installRow = operations.getByRole("row").filter({ hasText: "Install" });
  await expect(installRow.getByText("Waiting for the agent")).toBeVisible();
  await expect(installRow.getByText("Change 1")).toBeVisible();

  // On a phone the history stays readable (the table scrolls sideways).
  await page.setViewportSize({ width: 360, height: 780 });
  await expect(page.getByText("Scroll sideways to see all columns").first()).toBeVisible();
  await operations.scrollIntoViewIfNeeded();
  await expect(installRow.getByText("Waiting for the agent")).toBeVisible();
  for (const theme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: theme });
    await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${theme}\\b`));
    await expect(installRow.getByText("Waiting for the agent")).toBeVisible();
    if (process.env.ORGLANE_SCREENSHOTS) {
      await page.screenshot({
        path: `${repo}.github/pr-assets/apps/app-instance-waiting-mobile-${theme}.png`,
        fullPage: true,
      });
    }
  }
  await page.setViewportSize({ width: 1280, height: 900 });

  // The agent finished the install: the page refreshes to the result
  // (as agent_apply records it), and the waiting notice is gone.
  instance.sql(`UPDATE app_operations SET state = 'succeeded', error_code = NULL, finished_at = now()
    WHERE instance_id = '${instanceId}' AND generation = 1`);
  await expect(installRow.getByText("Succeeded")).toBeVisible({ timeout: 15_000 });
  await page.reload();
  await expect(page.getByText("To start the agent, run this on the server")).toHaveCount(0);
  if (process.env.ORGLANE_SCREENSHOTS) {
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.screenshot({
        path: `${repo}.github/pr-assets/apps/app-instance-operations-desktop-${theme}.png`,
        fullPage: true,
      });
    }
  }
});
