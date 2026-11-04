import type { Page } from "@playwright/test";
import { expect, type Instance, setUp, test } from "./fixtures";

const path = "**/api/v1/people-imports/github";
const problem = (status: number) => ({
  status,
  contentType: "application/problem+json",
  body: JSON.stringify({ type: "about:blank", title: "Controlled failure", status }),
});
async function start(page: Page, instance: Instance) {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/people/import`);
}
async function credentials(page: Page) {
  await page.getByLabel("Import source").selectOption("github");
  await page.getByLabel("Organization name").fill("fixture");
  await page.getByLabel("Short-lived fine-grained token").fill("synthetic-github-token");
}
test("late GitHub completion respects navigation and a new operation", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await start(page, instance);
  await credentials(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => {
    release = r;
  });
  let reached = false;
  await page.route(path, async (route) => {
    const response = await route.fetch();
    reached = true;
    await gate;
    await route.fulfill({ response });
  });
  try {
    await page.getByRole("button", { name: "Fetch members", exact: true }).click();
    await expect.poll(() => reached).toBe(true);
    await page.getByRole("link", { name: "People", exact: true }).click();
    await expect(page).toHaveURL(`${instance.url}/people`);
    release();
    await page.unrouteAll({ behavior: "wait" });
    // Flush the released browser response through a subsequent same-origin request.
    await page.request.get(`${instance.url}/api/v1/me`);
    await expect(page).toHaveURL(`${instance.url}/people`);
    await page.goto(`${instance.url}/people/import`);
    await credentials(page);
    await page.getByRole("button", { name: "Fetch members", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
  } finally {
    release();
    await page.unrouteAll({ behavior: "wait" });
  }
});
for (const prior of ["template", "upload"]) {
  test(`GitHub success clears prior ${prior} failure and preserves a fresh failure`, async ({
    page,
    instance,
    webauthn: _,
  }) => {
    await start(page, instance);
    if (prior === "template") {
      await page.route("**/api/v1/people-imports/template", (r) => r.fulfill(problem(503)));
      await page.getByRole("button", { name: "Download CSV template" }).click();
    } else {
      await page.getByLabel("CSV or XLSX file").setInputFiles({
        name: "malformed.csv",
        mimeType: "text/csv",
        buffer: Buffer.from('"unterminated'),
      });
      await page.getByRole("button", { name: "Upload file", exact: true }).click();
    }
    await expect(page.getByRole("alert")).toBeVisible();
    await credentials(page);
    await page.getByRole("button", { name: "Fetch members", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await page.route("**/api/v1/people-imports/*/preview", (r) => r.fulfill(problem(400)));
    await page.getByRole("button", { name: "Preview changes", exact: true }).click();
    await expect(page.getByRole("alert")).toBeVisible();
  });
}
test("GitHub pending feedback and disabled controls restore after failure and success", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await start(page, instance);
  await credentials(page);
  for (const fails of [true, false]) {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    let reached = false;
    await page.route(path, async (route) => {
      const response = fails ? undefined : await route.fetch();
      reached = true;
      await gate;
      await route.fulfill(response ? { response } : problem(400));
    });
    try {
      await page.getByRole("button", { name: "Fetch members", exact: true }).click();
      await expect.poll(() => reached).toBe(true);
      const fetch = page.getByRole("button", { name: "Fetch members", exact: true });
      await expect(fetch).toHaveAttribute("aria-busy", "true");
      await expect(fetch).toBeDisabled();
      await expect(page.getByRole("status")).toContainText("up to five minutes");
      await expect(page.getByLabel("Short-lived fine-grained token")).toHaveValue("");
      await expect(page.getByLabel("Short-lived fine-grained token")).toBeDisabled();
      await expect(page.getByLabel("Organization name")).toBeDisabled();
      await expect(page.getByLabel("Import source")).toBeDisabled();
    } finally {
      release();
      await page.unrouteAll({ behavior: "wait" });
    }
    if (fails) {
      await expect(page.getByRole("alert")).toContainText("GitHub fetch failed");
      await expect(
        page.getByRole("button", { name: "Fetch members", exact: true }),
      ).not.toHaveAttribute("aria-busy", "true");
      await expect(page.getByLabel("Short-lived fine-grained token")).toBeEnabled();
      await expect(page.getByRole("status")).toHaveCount(0);
      await credentials(page);
    } else {
      await expect(page.getByRole("heading", { name: "Map columns" })).toBeVisible();
      await expect(page.getByRole("alert")).toHaveCount(0);
      await expect(page.getByRole("status")).toHaveCount(0);
      await expect(
        page.getByRole("button", { name: "Preview changes", exact: true }),
      ).toBeEnabled();
    }
  }
});
