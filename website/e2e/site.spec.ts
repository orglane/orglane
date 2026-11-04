import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { expect, type Page, test } from "@playwright/test";

const installer = readFileSync(new URL("../../deploy/install.sh", import.meta.url));
const installerSha256 = createHash("sha256").update(installer).digest("hex");

/** Fails the test on requests to other origins, CSP violations and console errors. */
function watch(page: Page, origin: string): string[] {
  const problems: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.protocol !== "data:" && url.origin !== origin) problems.push(`request to ${url.href}`);
  });
  page.on("console", (message) => {
    if (message.type() === "error") problems.push(`console error: ${message.text()}`);
  });
  page.on("pageerror", (error) => problems.push(`page error: ${error.message}`));
  return problems;
}

test("the landing page shows the positioning, the pre-alpha notice and the install command", async ({
  page,
  baseURL,
}) => {
  const problems = watch(page, new URL(baseURL ?? "").origin);
  await page.goto("/");

  await expect(page).toHaveTitle(/open-source operating system for your company/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "The open-source operating system for your company",
  );
  await expect(page.getByText("Pre-alpha", { exact: true })).toBeVisible();
  await expect(page.getByRole("note")).toContainText("not ready for real company data");
  await expect(page.locator("#features li")).toHaveCount(5);
  // The vision is labelled as planned, and the install section says what
  // exists today.
  await expect(page.locator("#features li .tag--planned")).toHaveCount(5);
  await expect(page.locator("#features")).toContainText("the rest is in progress");
  await expect(page.getByRole("heading", { name: "What you get today" })).toBeVisible();
  await expect(page.locator(".command__code").first()).toHaveText(
    "curl -fsSL https://orglane.app/install.sh | sh",
  );
  await expect(page.getByTestId("installer-sha256")).toHaveText(installerSha256);
  // The verification command compares against the published checksum.
  await expect(page.locator(".command__code").nth(2)).toHaveText(
    `curl -fsSLO https://orglane.app/install.sh && echo "${installerSha256}  install.sh" | sha256sum -c`,
  );
  await expect(page.getByRole("link", { name: "View on GitHub" })).toHaveAttribute(
    "href",
    "https://github.com/orglane/orglane",
  );
  await expect(page.getByRole("contentinfo")).toContainText("AGPL-3.0-or-later");
  await expect(page.getByRole("contentinfo")).toContainText("trademarks");

  // Headings form an outline without skipped levels.
  const levels = await page
    .locator("h1, h2, h3")
    .evaluateAll((headings) => headings.map((h) => Number(h.tagName.slice(1))));
  expect(levels[0]).toBe(1);
  for (let i = 1; i < levels.length; i++) {
    expect((levels[i] ?? 0) - (levels[i - 1] ?? 0)).toBeLessThanOrEqual(1);
  }

  expect(problems).toEqual([]);
});

test("install.sh is the installer, byte for byte", async ({ request }) => {
  const response = await request.get("/install.sh");
  expect(response.ok()).toBe(true);
  // `astro preview` sends no Content-Type for .sh; GitHub Pages sends
  // application/x-sh (checked after each deployment by the website workflow).
  expect(response.headers()["content-type"] ?? "").not.toMatch(/html/);
  expect(Buffer.from(await response.body()).equals(installer)).toBe(true);
});

test("the copy button copies the install command", async ({ page, context, baseURL }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"], {
    origin: new URL(baseURL ?? "").origin,
  });
  await page.goto("/");
  const copy = page.getByRole("button", { name: "Copy the command: Install Orglane" });
  await copy.click();
  await expect(copy).toHaveText("Copied");
  await expect(page.getByRole("status")).toHaveText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    "curl -fsSL https://orglane.app/install.sh | sh",
  );
});

test("a failed copy says so and selects the command for copying by hand", async ({ page }) => {
  // A browser that has the Clipboard API but refuses the write (for example
  // a denied permission).
  await page.addInitScript(() => {
    Object.defineProperty(navigator.clipboard, "writeText", {
      value: () => Promise.reject(new DOMException("denied", "NotAllowedError")),
    });
  });
  await page.goto("/");
  const copy = page.getByRole("button", { name: "Copy the command: Install Orglane" });
  const error = page.locator(".command__error").first();
  await expect(error).toBeHidden();
  await copy.click();

  await expect(error).toBeVisible();
  await expect(error).toHaveText("Couldn’t copy. The command is selected: copy it by hand.");
  await expect(page.getByRole("status")).toHaveText(
    "Couldn’t copy. The command is selected: copy it by hand.",
  );
  await expect(copy).toHaveText("Copy");
  expect(await page.evaluate(() => getSelection()?.toString())).toBe(
    "curl -fsSL https://orglane.app/install.sh | sh",
  );
});

test("a failed copy right after a successful one resets the button label", async ({ page }) => {
  // The first write succeeds, the next one is refused, both within the
  // two seconds the "Copied" label is shown.
  await page.addInitScript(() => {
    let calls = 0;
    Object.defineProperty(navigator.clipboard, "writeText", {
      value: () =>
        calls++ === 0
          ? Promise.resolve()
          : Promise.reject(new DOMException("denied", "NotAllowedError")),
    });
  });
  await page.goto("/");
  const copy = page.getByRole("button", { name: "Copy the command: Install Orglane" });
  const error = page.locator(".command__error").first();
  await copy.click();
  await expect(copy).toHaveText("Copied");
  await expect(error).toBeHidden();

  await copy.click();
  await expect(error).toBeVisible();
  await expect(page.getByRole("status")).toHaveText(
    "Couldn’t copy. The command is selected: copy it by hand.",
  );
  await expect(copy).toHaveText("Copy");
  expect(await page.evaluate(() => getSelection()?.toString())).toBe(
    "curl -fsSL https://orglane.app/install.sh | sh",
  );
});

for (const colorScheme of ["light", "dark"] as const) {
  test(`fits a 360px wide screen without horizontal scrolling (${colorScheme})`, async ({
    page,
  }) => {
    await page.emulateMedia({ colorScheme });
    await page.setViewportSize({ width: 360, height: 780 });
    await page.goto("/");
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBe(0);
  });
}

test("unknown paths get the 404 page", async ({ page }) => {
  const response = await page.goto("/docs/");
  expect(response?.status()).toBe(404);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Page not found");
  await expect(page.getByRole("link", { name: "Go to the home page" })).toHaveAttribute(
    "href",
    "/",
  );
});
