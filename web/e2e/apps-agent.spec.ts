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

// A managed instance that ran 1.0.0-1, attempted the required stop to
// 2.0.0-1 and rolled back; a revert generation keeps desired and observed in
// sync (as apps.Service.operationFinished records it).
const rolledBack = (port: string) => `WITH i AS (
    INSERT INTO app_instances (key, provider, management, display_name, state, health, base_url, settings, access_policy, package_id, package_version, desired_generation, observed_generation)
    VALUES ('managed-test', 'agenttest', 'managed', 'Managed', 'active', 'healthy', 'http://agenttest.localhost:${port}', '{}', 'all_people', 'agenttest', '1.0.0-1', 2, 2)
    RETURNING id),
  d1 AS (INSERT INTO app_desired_specs (instance_id, generation, package_version, run_state, settings, credential_versions, reason)
    SELECT id, 1, '2.0.0-1', 'running', '{}', '{}', 'upgrade' FROM i),
  d2 AS (INSERT INTO app_desired_specs (instance_id, generation, package_version, run_state, settings, credential_versions, reason)
    SELECT id, 2, '1.0.0-1', 'running', '{}', '{}', 'revert' FROM i),
  o AS (INSERT INTO app_operations (instance_id, generation, kind, state, error_code, from_version, to_version, finished_at)
    SELECT id, 1, 'upgrade', 'rolled_back', 'aborted', '1.0.0-1', '2.0.0-1', now() FROM i)
  SELECT id FROM i`;

test("Settings → Apps reports a rolled-back upgrade and its retry", async ({ page, instance }) => {
  test.setTimeout(90_000);
  const port = new URL(instance.url).port;
  const instanceId = instance.sql(rolledBack(port)).split("\n")[0] ?? "";
  await signIn(page, instance.url);

  await page.goto(`${instance.url}/settings/apps/${instanceId}`);
  await expect(
    page.getByText("The upgrade to 2.0.0-1 was rolled back. The app is running 1.0.0-1."),
  ).toBeVisible();
  const operations = page.getByRole("table", { name: "Operations" });
  const upgradeRow = operations.getByRole("row").filter({ hasText: "Upgrade" });
  await expect(upgradeRow.getByText("1.0.0-1 → 2.0.0-1")).toBeVisible();
  await expect(upgradeRow.getByText("Rolled back")).toBeVisible();
  await expect(upgradeRow.getByText(/check of the new version failed/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry upgrade" })).toBeVisible();

  // The next poll shows the upgrade in progress instead: the instance is
  // upgrading and the agent waits for Orglane's health check.
  instance.sql(`UPDATE app_instances SET state = 'upgrading' WHERE id = '${instanceId}'`);
  instance.sql(`UPDATE app_operations SET state = 'awaiting_confirmation', error_code = NULL,
    phase = 'awaiting_confirmation', finished_at = NULL WHERE instance_id = '${instanceId}' AND generation = 1`);
  await page.reload();
  await expect(page.getByText("Upgrade to 2.0.0-1 is running.")).toBeVisible({ timeout: 15_000 });
  await expect(upgradeRow.getByText("Checking the new version")).toBeVisible();
  await expect(upgradeRow.getByText("Waiting for Orglane's check")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry upgrade" })).toHaveCount(0);
});
