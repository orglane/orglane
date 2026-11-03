import { mkdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, setUp, test } from "./fixtures";

const fifty = fileURLToPath(
  new URL("../../core/internal/peopleimport/testdata/fifty.csv", import.meta.url),
);

test("upload, map, preview and apply fifty people with persisted idempotent results", async ({
  page,
  instance,
  webauthn: _,
}, testInfo) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/people/import`);
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download CSV template" }).click();
  expect((await download).suggestedFilename()).toBe("people-template.csv");
  await page.getByLabel("CSV or XLSX file").setInputFiles(fifty);
  await page.getByRole("button", { name: "Upload file", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
  // The unfamiliar heading requires an explicit mapping. Source values are
  // never sent back to the browser, even in previews or error diagnostics.
  await page.getByLabel("Column 2", { exact: true }).selectOption("work_email");
  await page.getByRole("checkbox", { name: "Create teams that do not exist" }).check();
  const assetDir = fileURLToPath(
    new URL("../../.github/pr-assets/people-import/", import.meta.url),
  );
  mkdirSync(assetDir, { recursive: true });
  const mappingImage = `${assetDir}mapping.png`;
  await page.screenshot({ path: mappingImage, fullPage: true });
  await testInfo.attach("import-mapping", { path: mappingImage, contentType: "image/png" });
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "50 rows: 50 create, 0 update, 0 skip, 0 conflict.",
  );
  await expect(page.getByText("Import Person 01", { exact: true })).toHaveCount(0);
  const previewImage = `${assetDir}preview.png`;
  await page.screenshot({ path: previewImage, fullPage: true });
  await testInfo.attach("import-preview", { path: previewImage, contentType: "image/png" });
  const applied = page.waitForResponse(
    (r) => r.url().endsWith("/apply") && r.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  const response = await applied;
  expect(response.ok()).toBe(true);
  const result = (await response.json()) as {
    id: string;
    state: string;
    rows: { result: string }[];
  };
  expect(result.state).toBe("applied");
  expect(result.rows).toHaveLength(50);
  expect(result.rows.every((r) => r.result === "created")).toBe(true);
  await expect(page.getByRole("status")).toContainText(
    "Finished: 50 created, 0 updated, 0 skipped, 0 failed.",
  );
  const resultImage = `${assetDir}results.png`;
  await page.screenshot({ path: resultImage, fullPage: true });
  await testInfo.attach("import-results", { path: resultImage, contentType: "image/png" });
  const persisted = await page.request.get(`${instance.url}/api/v1/people-imports/${result.id}`);
  expect(await persisted.json()).toEqual(result);
  const me = (await (await page.request.get(`${instance.url}/api/v1/me`)).json()) as {
    csrfToken: string;
  };
  const repeated = await page.request.post(
    `${instance.url}/api/v1/people-imports/${result.id}/apply`,
    { headers: { Origin: instance.url, "X-CSRF-Token": me.csrfToken } },
  );
  expect(await repeated.json()).toEqual(result);
  const report = await page.request.get(
    `${instance.url}/api/v1/people-imports/${result.id}/errors`,
  );
  expect(await report.text()).toBe("row,decision,error,result\n");
  await page.getByRole("link", { name: "Manage invitations" }).click();
  await expect(page.getByText("Import Person 01", { exact: true })).toBeVisible();

  await page.goto(`${instance.url}/people/import`);
  await page
    .getByLabel("CSV or XLSX file")
    .setInputFiles({ name: "reimport.csv", mimeType: "text/csv", buffer: readFileSync(fifty) });
  await page.getByRole("button", { name: "Upload file", exact: true }).click();
  await page.getByLabel("Column 2", { exact: true }).selectOption("work_email");
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "50 rows: 0 create, 0 update, 50 skip, 0 conflict.",
  );
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "Finished: 0 created, 0 updated, 50 skipped, 0 failed.",
  );
});
