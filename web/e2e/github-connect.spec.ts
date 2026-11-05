import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import { expect, test } from "./fixtures";

test.use({ demo: true, reducedMotion: "reduce" });

test("an Admin prepares a GitHub App through the public API and posts its manifest to GitHub", async ({
  page,
  instance,
  browser,
}) => {
  const signedIn = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  expect(signedIn.ok()).toBeTruthy();
  await page.goto(`${instance.url}/settings/apps`);
  await page.getByRole("link", { name: "Connect GitHub", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Connect GitHub" })).toBeVisible();
  const evidence = process.env.ORGLANE_PR_ASSETS;
  if (evidence) {
    await mkdir(evidence, { recursive: true });
    await page.screenshot({
      path: join(evidence, "github-connect-desktop-light.png"),
      fullPage: true,
      animations: "disabled",
    });
  }
  await page.getByLabel("GitHub organization", { exact: true }).fill("orglane-test");
  await page.getByRole("button", { name: "Prepare GitHub App", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Create your GitHub App" })).toBeVisible();
  if (evidence) {
    await page.screenshot({
      path: join(evidence, "github-connect-ready-desktop-light.png"),
      fullPage: true,
      animations: "disabled",
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.screenshot({
      path: join(evidence, "github-connect-ready-mobile-dark.png"),
      fullPage: true,
      animations: "disabled",
    });
  }
  const form = page.locator('form[action^="https://github.com/organizations/"]');
  const action = new URL((await form.getAttribute("action")) ?? "");
  expect(action.origin).toBe("https://github.com");
  expect(action.pathname).toBe("/organizations/orglane-test/settings/apps/new");
  const state = action.searchParams.get("state");
  expect(state).toMatch(/^ols_[A-Za-z0-9_-]{43}$/);
  const manifest = JSON.parse(await form.locator('input[name="manifest"]').inputValue());
  expect(manifest.default_permissions).toEqual({
    members: "write",
    metadata: "read",
    pull_requests: "read",
    issues: "read",
  });
  expect(manifest.redirect_url).toBe(`${instance.url}/settings/apps/github/callback`);
  expect(manifest.setup_url).toBe(manifest.redirect_url);
  expect(manifest.hook_attributes.url).toMatch(
    new RegExp(`^${instance.url}/api/v1/webhooks/[0-9a-f-]+$`),
  );
  const row = instance.sql(
    "SELECT state,settings->>'organization',management FROM app_instances WHERE key='github'",
  );
  expect(row).toBe("connecting|orglane-test|connected");
  // Stop at the external browser handoff. No GitHub provider API is mocked,
  // and no real organization is created or modified by this browser test.
  let posted = "";
  await page.route(
    "https://github.com/organizations/orglane-test/settings/apps/new?*",
    async (route) => {
      expect(route.request().method()).toBe("POST");
      posted = route.request().postData() ?? "";
      await route.fulfill({
        status: 200,
        contentType: "text/html",
        body: "<!doctype html><title>External handoff captured</title>",
      });
    },
  );
  await page.getByRole("button", { name: "Continue to GitHub", exact: true }).click();
  await expect(page).toHaveURL(action.href);
  expect(new URLSearchParams(posted).get("manifest")).toBe(JSON.stringify(manifest));
  const context = await browser.newContext();
  try {
    expect(
      (
        await context.request.post(`${instance.url}/api/v1/demo/sign-in`, {
          data: { username: "priya.raman" },
          headers: { Origin: instance.url },
        })
      ).ok(),
    ).toBeTruthy();
    const member = await context.newPage();
    await member.goto(`${instance.url}/settings/apps/github/connect`);
    await expect(member.getByRole("alert")).toContainText("permission to manage apps");
    await expect(member.getByRole("button", { name: "Prepare GitHub App" })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("a malformed GitHub callback is explained without replaying a code", async ({
  page,
  instance,
}) => {
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
        data: { username: "lucas.ferreira" },
        headers: { Origin: instance.url },
      })
    ).ok(),
  ).toBeTruthy();
  let calls = 0;
  page.on("request", (r) => {
    if (r.url().endsWith("/api/v1/apps/github/connect/complete")) calls++;
  });
  await page.goto(
    `${instance.url}/settings/apps/github/callback?state=ols_one&state=ols_two&code=never-send`,
  );
  await expect(page.getByRole("alert")).toContainText("could not be completed");
  await expect(page).toHaveURL(`${instance.url}/settings/apps/github/callback`);
  expect(calls).toBe(0);
  await expect(page.getByRole("link", { name: "Back to Apps" })).toBeVisible();
});

test("an Admin can resume a prepared instance and inspect manual setup without submitting credentials", async ({
  page,
  instance,
}) => {
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
        data: { username: "lucas.ferreira" },
        headers: { Origin: instance.url },
      })
    ).ok(),
  ).toBeTruthy();
  await page.goto(`${instance.url}/settings/apps/github/connect`);
  await page.getByLabel("GitHub organization", { exact: true }).fill("orglane-test");
  await page.getByRole("button", { name: "Prepare GitHub App", exact: true }).click();
  await page.getByRole("link", { name: "View connection", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Resume GitHub connection", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("GitHub organization", { exact: true })).toBeDisabled();
  await expect(page.getByText(/\/api\/v1\/webhooks\//)).toBeVisible();
  await page.getByRole("button", { name: "Use manual setup", exact: true }).click();
  await expect(page.getByLabel("Private key (PEM)", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Client secret", { exact: true })).toHaveAttribute(
    "type",
    "password",
  );
  const evidence = process.env.ORGLANE_PR_ASSETS;
  if (evidence) {
    await mkdir(evidence, { recursive: true });
    await page.screenshot({
      path: join(evidence, "github-connect-manual-desktop-light.png"),
      fullPage: true,
      animations: "disabled",
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.screenshot({
      path: join(evidence, "github-connect-manual-mobile-dark.png"),
      fullPage: true,
      animations: "disabled",
    });
  }
});

test("reserved keys and invalid manual PEM explain recovery at their retained fields", async ({
  page,
  instance,
}) => {
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
        data: { username: "lucas.ferreira" },
        headers: { Origin: instance.url },
      })
    ).ok(),
  ).toBeTruthy();
  await page.goto(`${instance.url}/settings/apps/github/connect`);
  await page.getByLabel("GitHub organization", { exact: true }).fill("orglane-test");
  await page.getByRole("button", { name: "Prepare GitHub App", exact: true }).click();
  const connection = await page
    .getByRole("link", { name: "View connection", exact: true })
    .getAttribute("href");
  await page.goto(`${instance.url}/settings/apps/github/connect`);
  await page.getByLabel("GitHub organization", { exact: true }).fill("orglane-test");
  await page.getByRole("button", { name: "Prepare GitHub App", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Choose a different app key");
  await expect(page.getByLabel("App key in Orglane")).toHaveAccessibleDescription(
    /Choose a different app key/,
  );
  await expect(page.getByLabel("GitHub organization", { exact: true })).toHaveValue("orglane-test");
  const assets = process.env.ORGLANE_PR_ASSETS;
  if (assets) {
    await mkdir(assets, { recursive: true });
    await page.screenshot({
      path: join(assets, "github-key-reserved-desktop-light.png"),
      fullPage: true,
    });
  }
  await page.goto(`${instance.url}${connection}`);
  await page.getByRole("button", { name: "Use manual setup", exact: true }).click();
  for (const [label, value] of [
    ["App ID", "123"],
    ["Client ID", "client"],
    ["Client secret", "retained-secret"],
    ["Webhook secret (at least 32 characters)", "x".repeat(32)],
    ["Private key (PEM)", "invalid-local-fixture"],
  ] as const)
    await page.getByLabel(label, { exact: true }).fill(value);
  await page.getByRole("button", { name: "Resume GitHub connection", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Generate a private key in this GitHub App");
  await expect(page.getByLabel("Private key (PEM)")).toHaveAccessibleDescription(
    /Generate a private key/,
  );
  await expect(page.getByLabel("Private key (PEM)")).toHaveValue("invalid-local-fixture");
  await expect(page.getByLabel("Client secret", { exact: true })).toHaveValue("retained-secret");
  if (assets) {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.emulateMedia({ colorScheme: "dark" });
    await expect(page.locator("html")).toHaveClass(/dark/);
    await page.screenshot({
      path: join(assets, "github-invalid-key-mobile-dark.png"),
      fullPage: true,
    });
  }
});

test("a rejected replacement stays explained through polling and reload while the working key stays active", async ({
  page,
  instance,
}) => {
  // Synthetic metadata is only for display/local parser coverage, not a connected App.
  const id = instance
    .sql(
      `INSERT INTO app_instances(key,provider,management,display_name,state,health,access_policy,settings) VALUES('github-fixture','github','connected','GitHub local fixture','active','healthy','mapped_teams','{"organization":"orglane-test","organization_id":"123","app_id":"456"}') RETURNING id`,
    )
    .split("\n")[0];
  instance.sql(
    `INSERT INTO app_credentials(instance_id,name,version,ciphertext,state,activated_at) VALUES('${id}','app_private_key',1,'\\x00','active',now())`,
  );
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
        data: { username: "lucas.ferreira" },
        headers: { Origin: instance.url },
      })
    ).ok(),
  ).toBeTruthy();
  await page.goto(`${instance.url}/settings/apps/${id}`);
  await page.getByLabel("New private key (.pem)").setInputFiles({
    name: "invalid.pem",
    mimeType: "application/x-pem-file",
    buffer: Buffer.from("invalid-local-fixture"),
  });
  await page.getByRole("button", { name: "Verify replacement key", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Replacement key verification failed", {
    timeout: 15000,
  });
  await expect(page.getByRole("alert")).toContainText("The working key is still active");
  expect(
    instance
      .sql(`SELECT version FROM app_credentials WHERE instance_id='${id}' AND state='active'`)
      .split("\n")[0],
  ).toBe("1");
  expect(
    instance
      .sql(`SELECT version FROM app_credentials WHERE instance_id='${id}' AND state='discarded'`)
      .split("\n")[0],
  ).toBe("2");
  await page.reload();
  await expect(page.getByRole("alert")).toContainText("Replacement key verification failed");
  await expect(page.getByLabel("New private key (.pem)")).toBeEnabled();
  await expect(
    page.getByText("Replacement key queued for verification.", { exact: true }),
  ).toHaveCount(0);
  await expect(page.getByText(/Replacement key accepted and queued for verification/)).toHaveCount(
    0,
  );
  const assets = process.env.ORGLANE_PR_ASSETS;
  if (assets) {
    await mkdir(assets, { recursive: true });
    for (const [width, theme] of [
      [1440, "light"],
      [390, "dark"],
    ] as const) {
      await page.setViewportSize({ width, height: width === 1440 ? 900 : 844 });
      await page.emulateMedia({ colorScheme: theme });
      await expect(page.locator("html")).toHaveClass(new RegExp(theme));
      await page.screenshot({
        path: join(assets, `github-replacement-failed-${width}-${theme}.png`),
        fullPage: true,
      });
    }
  }
});
