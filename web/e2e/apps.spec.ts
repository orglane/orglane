import type { Page } from "@playwright/test";
import { repo } from "./env";
import { expect, test } from "./fixtures";

test.use({ demo: true });

// The app instance an install (#61) or connect (#64) flow would create; the
// test build compiles in the in-repo test provider (build tag appstest).
const createInstance = `INSERT INTO app_instances (key, provider, management, display_name, state, base_url, settings, access_policy, health)
  VALUES ('test', 'testprovider', 'connected', 'Code', 'active', 'https://code.acme.test',
          '{"organization": "acme", "require_link": false}', 'mapped_teams', 'healthy')
  RETURNING id`;

test("an Admin sees Settings → Apps, maps a team and its members get access; a Member cannot manage apps", async ({
  page,
  instance,
  browser,
}) => {
  test.setTimeout(90_000);
  const instanceId = instance.sql(createInstance).split("\n")[0] ?? "";
  instance.sql(
    `INSERT INTO app_bindings (capability, instance_id, is_default) VALUES ('code', '${instanceId}', true)`,
  );

  const signedIn = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  expect(signedIn.ok()).toBeTruthy();
  await page.goto(`${instance.url}/`);
  await page.getByRole("button", { name: /^Account menu for / }).click();
  await page.getByRole("menuitem", { name: "Apps and access" }).click();
  await expect(page.getByRole("heading", { name: "Apps", exact: true, level: 1 })).toBeVisible();

  const instances = page.getByRole("table", { name: "Connected apps" });
  const row = instances.getByRole("row").filter({ hasText: "Code" });
  await expect(row.getByText("Active")).toBeVisible();
  await expect(row.getByText("Default for code")).toBeVisible();
  const matrix = page.getByRole("table", { name: "Capability matrix" });
  await expect(matrix.getByRole("columnheader", { name: /Test provider/ })).toBeVisible();
  await expect(matrix.getByRole("rowheader", { name: "Suspend accounts" })).toBeVisible();
  await expect(
    matrix.getByRole("row", { name: /Suspend accounts/ }).getByText("Planned (M5)"),
  ).toBeVisible();

  await row.getByRole("link", { name: "Code" }).click();
  await expect(page.getByRole("heading", { name: "Code", level: 1 })).toBeVisible();
  await expect(page.getByText("Nobody has access yet.")).toBeVisible();
  await expect(page.getByText(`/api/v1/webhooks/${instanceId}`)).toBeVisible();

  await page.getByLabel("Team to map").selectOption({ label: "Platform" });
  await page.getByRole("button", { name: "Map team" }).click();
  await expect(page.getByRole("button", { name: "Remove the mapping of Platform" })).toBeVisible();
  // The jobs sync the team and provision its members in the background.
  await expect(async () => {
    await page.reload();
    await expect(page.getByText("Waiting for sync")).toHaveCount(0, { timeout: 1000 });
    await expect(
      page
        .getByRole("table", { name: "People" })
        .getByRole("row")
        .filter({ hasText: "Lucas Ferreira" })
        .getByText("Up to date"),
    ).toBeVisible({ timeout: 1000 });
    await expect(page.getByText(/^5 up to date/)).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 45_000 });

  // The dry run plans nothing more.
  await page.getByRole("button", { name: "Dry run" }).first().click();
  await expect(page.getByText("Everything is up to date.")).toBeVisible();

  // The Admin's own apps.
  await page.goto(`${instance.url}/apps`);
  const card = page.getByRole("listitem").filter({ hasText: "Code" });
  await expect(card.getByText("Up to date")).toBeVisible();
  await expect(card.getByRole("link", { name: "Open" })).toHaveAttribute(
    "href",
    "https://code.acme.test",
  );

  // A person's page shows their app access to people managers.
  await page.goto(`${instance.url}/people`);
  await page.getByRole("link", { name: "Priya Raman" }).first().click();
  const apps = page
    .getByRole("heading", { name: "Apps and accounts" })
    .locator("xpath=ancestor::div[contains(@class,'rounded-xl')][1]");
  await expect(apps.getByRole("link", { name: "Code" })).toBeVisible();
  await expect(apps.getByText("Verified")).toBeVisible();

  const context = await browser.newContext();
  try {
    const member = await context.newPage();
    expect(
      (
        await context.request.post(`${instance.url}/api/v1/demo/sign-in`, {
          data: { username: "priya.raman" },
          headers: { Origin: instance.url },
        })
      ).ok(),
    ).toBeTruthy();
    await member.goto(`${instance.url}/settings/apps`);
    await expect(member.getByRole("alert")).toContainText("permission to manage apps");
    await member.goto(`${instance.url}/apps`);
    await expect(
      member.getByRole("listitem").filter({ hasText: "Code" }).getByText("Up to date"),
    ).toBeVisible();
    await expect(member.getByRole("link", { name: "Manage apps" })).toHaveCount(0);
    // Not on Platform: no access, no app.
    const outsider = await browser.newContext();
    try {
      const page2 = await outsider.newPage();
      await outsider.request.post(`${instance.url}/api/v1/demo/sign-in`, {
        data: { username: "fatima.alsayed" },
        headers: { Origin: instance.url },
      });
      await page2.goto(`${instance.url}/apps`);
      await expect(page2.getByText("No apps assigned to you yet")).toBeVisible();
    } finally {
      await outsider.close();
    }
  } finally {
    await context.close();
  }
});

// Review captures for the PR (not committed):
//   ORGLANE_SCREENSHOTS=1 pnpm --filter @orglane/web test:e2e apps
// writes .github/pr-assets/apps/<screen>-<viewport>-<theme>.png.
test("apps screenshots", async ({ page, instance, browser }) => {
  test.skip(!process.env.ORGLANE_SCREENSHOTS, "set ORGLANE_SCREENSHOTS=1 to capture screenshots");
  test.setTimeout(240_000);
  const instanceId = instance.sql(createInstance).split("\n")[0] ?? "";
  instance.sql(
    `INSERT INTO app_bindings (capability, instance_id, is_default) VALUES ('code', '${instanceId}', true)`,
  );
  await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  await page.goto(`${instance.url}/settings/apps/${instanceId}`);
  await page.getByLabel("Team to map").selectOption({ label: "Platform" });
  await page.getByRole("button", { name: "Map team" }).click();
  // The page refreshes by itself while the jobs run.
  await expect(page.getByText(/^5 up to date/)).toBeVisible({ timeout: 45_000 });

  const priya = await page.request.get(`${instance.url}/api/v1/people?q=priya`);
  const priyaId = ((await priya.json()) as { items: { id: string }[] }).items[0]?.id ?? "";
  const viewports = [
    ["desktop", { width: 1280, height: 900 }],
    ["tablet", { width: 768, height: 1024 }],
    ["mobile", { width: 360, height: 780 }],
  ] as const;
  async function shoot(target: Page, name: string, path: string) {
    for (const [viewport, size] of viewports) {
      await target.setViewportSize(size);
      await target.goto(`${instance.url}${path}`);
      await expect(target.getByRole("heading", { level: 1 })).toBeVisible();
      if (name === "person-apps") {
        await target.getByRole("heading", { name: "Apps and accounts" }).scrollIntoViewIfNeeded();
      }
      for (const theme of ["light", "dark"] as const) {
        await target.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
        await expect(target.locator("html")).toHaveClass(new RegExp(`\\b${theme}\\b`));
        await target.screenshot({
          path: `${repo}.github/pr-assets/apps/${name}-${viewport}-${theme}.png`,
          animations: "disabled",
          fullPage: name !== "person-apps",
        });
      }
    }
  }
  await shoot(page, "settings-apps", "/settings/apps");
  await shoot(page, "app-instance", `/settings/apps/${instanceId}`);
  await shoot(page, "my-apps", "/apps");
  await shoot(page, "person-apps", `/people/${priyaId}`);

  // Problems: the agent is unreachable, a rotation has waited for a day and
  // one person's access failed.
  instance.sql(
    `UPDATE app_instances SET state = 'degraded', health = 'unhealthy', health_code = 'agent_unavailable' WHERE id = '${instanceId}'`,
  );
  instance.sql(`INSERT INTO app_credentials (instance_id, name, version, ciphertext, state, created_at, activated_at) VALUES
    ('${instanceId}', 'webhook_secret', 1, '\\x00', 'active', now() - interval '3 days', now() - interval '3 days'),
    ('${instanceId}', 'webhook_secret', 2, '\\x00', 'uncertain', now() - interval '25 hours', NULL)`);
  instance.sql(`UPDATE provisioning_results SET status = 'failed', error_code = 'seat_limit'
    WHERE instance_id = '${instanceId}' AND person_id = (SELECT id FROM people WHERE username = 'sofia.petrova')`);
  await shoot(page, "app-instance-problems", `/settings/apps/${instanceId}`);

  const context = await browser.newContext();
  try {
    const outsider = await context.newPage();
    await context.request.post(`${instance.url}/api/v1/demo/sign-in`, {
      data: { username: "fatima.alsayed" },
      headers: { Origin: instance.url },
    });
    await shoot(outsider, "my-apps-empty", "/apps");
  } finally {
    await context.close();
  }
});
