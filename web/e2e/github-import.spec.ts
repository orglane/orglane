import { mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, setUp, test } from "./fixtures";

test("GitHub source fetch, missing email, pending import and repeat", async ({
  page,
  instance,
  webauthn: _,
}, testInfo) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  const begin = async () => {
    await page.goto(`${instance.url}/people/import`);
    await page.getByLabel("Import source").selectOption("github");
    await page.getByLabel("Organization name").fill("fixture");
    await page.getByLabel("Short-lived fine-grained token").fill("synthetic-github-token");
    await page.getByRole("button", { name: "Fetch members" }).click();
    await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
    await page.getByLabel("Column 3", { exact: true }).selectOption("work_email");
  };
  await begin();
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "2 rows: 1 create, 0 update, 0 skip, 1 conflict.",
  );
  const assets = fileURLToPath(new URL("../../.github/pr-assets/github-import/", import.meta.url));
  mkdirSync(assets, { recursive: true });
  const screenshot = `${assets}preview.png`;
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("github-import-preview", { path: screenshot, contentType: "image/png" });
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("1 created");
  await expect(page.getByRole("status")).toContainText("1 failed");
  // Credentials never enter browser persistent storage or cached source results.
  const storage = await page.evaluate(() => JSON.stringify([localStorage, sessionStorage]));
  expect(storage).not.toContain("synthetic-github-token");
  await begin();
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "2 rows: 0 create, 0 update, 1 skip, 1 conflict.",
  );
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("0 created");
  await page.getByRole("link", { name: "Manage invitations" }).click();
  await expect(page.getByText("GitHub Fixture Person", { exact: true })).toBeVisible();
});
