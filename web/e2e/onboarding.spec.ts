import { mkdir } from "node:fs/promises";
import { join } from "node:path";

import { repo } from "./env";
import { expect, owner, setUp, test } from "./fixtures";

test("invited person completes required profile, skips optional steps, and progress stays live", async ({
  page,
  browser,
  instance,
  webauthn: _,
}) => {
  const screenshotDirectory = join(repo, ".github/pr-assets/onboarding");
  await mkdir(screenshotDirectory, { recursive: true });
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  const ownerMe = await (await page.request.get(`${instance.url}/api/v1/me`)).json();
  const configured = await page.request.post(`${instance.url}/api/v1/profile-fields`, {
    headers: { Origin: instance.url, "X-CSRF-Token": ownerMe.csrfToken },
    data: { key: "office", label: "Office", type: "text", requiredAtOnboarding: true },
  });
  expect(configured.status()).toBe(201);
  await page.goto(`${instance.url}/people/invitations`);
  await page.getByLabel("Full name").fill("Maya Okafor");
  await page.getByLabel("Contact email").fill("maya@example.test");
  await page.getByLabel("Username").fill("maya");
  await page.getByRole("button", { name: "Invite person", exact: true }).click();
  const link = await page.getByLabel("Invitation link").inputValue();
  const invited = await (await page.request.get(`${instance.url}/api/v1/people`)).json();
  const personId = invited.items.find((p: { username: string }) => p.username === "maya").id;
  const context = await browser.newContext({ reducedMotion: "reduce" });
  const employee = await context.newPage();
  await employee.goto(link);
  await employee.getByLabel("Password", { exact: true }).fill(owner.password);
  await employee.getByRole("button", { name: "Accept invitation", exact: true }).click();
  // Preserve the established passkey offer and its explicit optional Skip.
  await expect(
    employee.getByRole("heading", { name: "Sign in faster with a passkey" }),
  ).toBeVisible();
  await employee.getByRole("button", { name: "Skip for now" }).click();
  await expect(employee.getByRole("heading", { name: "Welcome to your workspace" })).toBeVisible();
  await expect(
    employee.getByRole("button", { name: "Skip optional steps and continue" }),
  ).toBeDisabled();
  await expect(employee.getByText("Orglane mobile", { exact: false })).toHaveCount(0);
  await employee.screenshot({
    path: join(screenshotDirectory, "onboarding-welcome-required.png"),
    fullPage: true,
    animations: "disabled",
  });
  await employee.setViewportSize({ width: 360, height: 780 });
  await employee.reload();
  await expect(
    employee.getByRole("button", { name: "Skip optional steps and continue" }),
  ).toBeDisabled();
  await employee.evaluate(() => document.fonts.ready);
  await employee.screenshot({
    path: join(screenshotDirectory, "onboarding-welcome-narrow.png"),
    fullPage: true,
    animations: "disabled",
  });
  await employee.setViewportSize({ width: 1280, height: 720 });
  await employee.reload();
  await expect(employee.getByRole("heading", { name: "Welcome to your workspace" })).toBeVisible();
  const field = employee
    .getByRole("listitem")
    .filter({ has: employee.getByRole("heading", { name: "Office", exact: true }) });
  await field.getByRole("button", { name: "Edit", exact: true }).click();
  await field.getByLabel("Office", { exact: true }).fill("Lisbon");
  await field.getByLabel("Office", { exact: true }).press("Tab");
  await expect(field.getByRole("button", { name: "Save", exact: true })).toBeFocused();
  await employee.keyboard.press("Enter");
  await expect(employee.getByTestId("onboarding-profile")).toContainText("Complete · Required");
  await expect(
    employee.getByRole("button", { name: "Skip optional steps and continue" }),
  ).toBeEnabled();
  await expect(
    employee.getByRole("button", { name: "Skip optional steps and continue" }),
  ).toBeFocused();
  const completion = employee.getByTestId("onboarding-profile").getByRole("status");
  await expect(completion).toHaveText("Complete · Required");
  await expect(completion).toHaveAttribute("aria-live", "polite");
  await employee.screenshot({
    path: join(screenshotDirectory, "onboarding-welcome-complete.png"),
    fullPage: true,
    animations: "disabled",
  });
  await employee.getByRole("button", { name: "Skip optional steps and continue" }).click();
  await expect(employee.getByRole("heading", { level: 1 })).toContainText("Maya");
  await expect(employee.getByTestId("onboarding-passkey")).toContainText("To do");
  await expect(employee.getByTestId("onboarding-avatar")).toContainText("To do");
  await employee.screenshot({
    path: join(screenshotDirectory, "onboarding-home-skipped.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.goto(`${instance.url}/people/${personId}`);
  await expect(
    page.getByRole("heading", { name: "Onboarding progress", exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId("onboarding-profile")).toContainText("Complete · Required");
  await page.screenshot({
    path: join(screenshotDirectory, "onboarding-admin-progress.png"),
    fullPage: true,
    animations: "disabled",
  });
  // Repeated sign-in preserves a finished welcome, while actual profile changes update Home.
  await employee.goto(`${instance.url}/settings/security`);
  await employee.getByRole("button", { name: "Account menu for Maya Okafor" }).click();
  await employee.getByRole("menuitem", { name: "Sign out" }).click();
  await employee.getByLabel("Work email").fill("maya@localhost");
  await employee.getByLabel("Password", { exact: true }).fill(owner.password);
  await employee.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(employee.getByRole("heading", { level: 1 })).toContainText("Maya");
  // The optional photo action reaches the directory's real, authorized upload control.
  await employee
    .getByTestId("onboarding-avatar")
    .getByRole("link", { name: "Open step: Upload a photo" })
    .click();
  await employee.getByLabel("Upload avatar", { exact: true }).setInputFiles({
    name: "avatar.png",
    mimeType: "image/png",
    buffer: Buffer.from(
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQaPjwHwAE5AKQBYzgxgAAAABJRU5ErkJggg==",
      "base64",
    ),
  });
  await expect(employee.getByText("Avatar updated.", { exact: true })).toBeVisible();
  await employee.goto(instance.url);
  await expect(employee.getByTestId("onboarding-avatar")).toContainText("Complete");
  await employee.goto(`${instance.url}/people/${personId}`);
  const office = employee
    .getByRole("listitem")
    .filter({ has: employee.getByRole("heading", { name: "Office", exact: true }) });
  await office.getByRole("button", { name: "Edit", exact: true }).click();
  await office.getByRole("button", { name: "Clear value", exact: true }).click();
  await expect(office.getByTestId("profile-value-office")).toHaveText("—");
  await employee.goto(instance.url);
  await expect(employee.getByTestId("onboarding-profile")).toContainText("To do");
  await expect(employee.getByRole("link", { name: "Continue welcome" })).toHaveCount(0);
  await context.close();
});
