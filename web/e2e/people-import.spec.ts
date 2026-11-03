import { mkdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { Page } from "@playwright/test";
import { expect, type Instance, setUp, test } from "./fixtures";

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

async function mappedImport(page: Page, instance: Instance) {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/people/import`);
  await page.getByLabel("CSV or XLSX file").setInputFiles(fifty);
  await page.getByRole("button", { name: "Upload file", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
  await page.getByLabel("Column 2", { exact: true }).selectOption("work_email");
  await page.getByRole("checkbox", { name: "Create teams that do not exist" }).check();
}

test("mapping controls stay frozen while a preview response is pending", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await mappedImport(page, instance);
  let release: (() => void) | undefined;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reached = false;
  await page.route("**/api/v1/people-imports/*/preview", async (route) => {
    const response = await route.fetch();
    reached = true;
    await blocked;
    await route.fulfill({ response });
  });
  try {
    await page.getByRole("button", { name: "Preview changes", exact: true }).click();
    await expect.poll(() => reached).toBe(true);
    await expect(page.getByLabel("Column 2", { exact: true })).toBeDisabled();
    await expect(
      page.getByRole("button", { name: "Preview changes", exact: true }),
    ).toHaveAttribute("aria-busy", "true");
    await expect(
      page.getByRole("checkbox", { name: "Create teams that do not exist" }),
    ).toBeDisabled();
    await expect(
      page.getByRole("checkbox", { name: "Email invitations for newly created people" }),
    ).toBeDisabled();
  } finally {
    release?.();
    await page.unrouteAll({ behavior: "wait" });
  }
  await expect(page.getByRole("status")).toContainText(
    "50 rows: 50 create, 0 update, 0 skip, 0 conflict.",
  );
  await expect(page.getByRole("checkbox", { name: "Skip row 2", exact: true })).toBeEnabled();
});

test("skip controls stay frozen while an apply response is pending", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await mappedImport(page, instance);
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "50 rows: 50 create, 0 update, 0 skip, 0 conflict.",
  );
  let release: (() => void) | undefined;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reached = false;
  await page.route("**/api/v1/people-imports/*/apply", async (route) => {
    const response = await route.fetch();
    reached = true;
    await blocked;
    await route.fulfill({ response });
  });
  try {
    await page.getByRole("button", { name: "Apply import", exact: true }).click();
    await expect.poll(() => reached).toBe(true);
    const skip = page.getByRole("checkbox", { name: "Skip row 2", exact: true });
    await expect(skip).toBeDisabled();
    await expect(skip).not.toBeChecked();
    await expect(page.getByRole("button", { name: "Apply import", exact: true })).toHaveAttribute(
      "aria-busy",
      "true",
    );
    await expect(page.getByRole("button", { name: "Edit mapping", exact: true })).toBeDisabled();
  } finally {
    release?.();
    await page.unrouteAll({ behavior: "wait" });
  }
  await expect(page.getByRole("status")).toContainText(
    "Finished: 50 created, 0 updated, 0 skipped, 0 failed.",
  );
});

const controlledProblem = (status: number) => ({
  status,
  contentType: "application/problem+json",
  body: JSON.stringify({ type: "about:blank", title: "Controlled request failure", status }),
});

test("recovery clears superseded apply errors and preserves a new failed request", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await mappedImport(page, instance);
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "50 rows: 50 create, 0 update, 0 skip, 0 conflict.",
  );
  const applyPath = "**/api/v1/people-imports/*/apply";
  await page.route(applyPath, (route) => route.fulfill(controlledProblem(409)));
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("needs a new preview");

  await page.getByRole("button", { name: "Edit mapping", exact: true }).click();
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Review changes", exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);

  // A later failure is current and must remain visible until actual recovery.
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("needs a new preview");
  await page.unroute(applyPath);
  await page.getByRole("button", { name: "Edit mapping", exact: true }).click();
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "Finished: 50 created, 0 updated, 0 skipped, 0 failed.",
  );
});

test("recovery clears superseded download errors while fresh request failures remain", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/people/import`);
  const templatePath = "**/api/v1/people-imports/template";
  await page.route(templatePath, (route) => route.fulfill(controlledProblem(503)));
  await page.getByRole("button", { name: "Download CSV template", exact: true }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await page.unroute(templatePath);
  await page.getByLabel("CSV or XLSX file").setInputFiles(fifty);
  await page.getByRole("button", { name: "Upload file", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByLabel("Column 2", { exact: true }).selectOption("work_email");
  await page.getByRole("checkbox", { name: "Create teams that do not exist" }).check();

  const previewPath = "**/api/v1/people-imports/*/preview";
  await page.route(previewPath, (route) => route.fulfill(controlledProblem(400)));
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await page.unroute(previewPath);
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Review changes", exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("button", { name: "Apply import", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "Finished: 50 created, 0 updated, 0 skipped, 0 failed.",
  );

  const exportPath = "**/api/v1/people-imports/*/errors";
  await page.route(exportPath, (route) => route.fulfill(controlledProblem(503)));
  await page.getByRole("button", { name: "Download error report", exact: true }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await page.getByRole("button", { name: "Import another file", exact: true }).click();
  await expect(page.getByLabel("CSV or XLSX file")).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});
