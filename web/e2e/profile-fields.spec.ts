import type { Page } from "@playwright/test";
import { expect, test } from "./fixtures";

test.use({ demo: true });

async function signIn(page: Page, url: string, username: string) {
  const response = await page.request.post(`${url}/api/v1/demo/sign-in`, {
    data: { username },
    headers: { Origin: url },
  });
  expect(response.ok()).toBeTruthy();
  return (await response.json()) as { person: { id: string }; csrfToken: string };
}

test("admin configures fields; members see only authorized values and no birth year", async ({
  page,
  browser,
  instance,
}) => {
  await signIn(page, instance.url, "lucas.ferreira");
  await page.goto(`${instance.url}/settings/profile-fields`);
  await expect(page.getByRole("heading", { name: "Profile fields", exact: true })).toBeVisible();
  for (const name of ["Telegram", "Birthday"]) {
    const toggle = page.getByRole("switch", { name, exact: true });
    await toggle.click();
    await expect(toggle).toBeChecked();
  }
  await page.getByRole("button", { name: "Create custom field" }).click();
  await page.getByLabel("Field name").fill("Office");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await page.getByLabel("Field type").selectOption("select");
  await page.getByLabel("Choices (one per line)").fill("Berlin\nLisbon");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  const wizard = page.getByRole("form", { name: "Create custom field" });
  await wizard.getByLabel("Visibility", { exact: true }).selectOption("admins");
  await expect(wizard.getByText("Required at onboarding")).not.toBeVisible();
  await page.getByRole("button", { name: "Create field", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Office", exact: true })).toBeVisible();

  await page.screenshot({
    path: test.info().outputPath("profile-fields-settings.png"),
    fullPage: true,
    animations: "disabled",
  });

  // Separate real sessions seed a member's profile through the same public API.
  const ownerContext = await browser.newContext();
  const ownerPage = await ownerContext.newPage();
  const owner = await signIn(ownerPage, instance.url, "maya.okafor");
  const memberContext = await browser.newContext();
  const memberPage = await memberContext.newPage();
  const member = await signIn(memberPage, instance.url, "priya.raman");
  const result = await ownerPage.request.patch(
    `${instance.url}/api/v1/people/${member.person.id}/profile`,
    {
      headers: { Origin: instance.url, "X-CSRF-Token": owner.csrfToken },
      data: {
        values: [
          { key: "telegram", value: "@priya_raman" },
          { key: "birthday", value: "1990-03-14" },
          { key: "office", value: "berlin" },
        ],
      },
    },
  );
  expect(result.ok()).toBeTruthy();
  await page.goto(`${instance.url}/people/${member.person.id}`);
  await expect(page.getByTestId("profile-value-office")).toHaveText("Berlin");
  await expect(page.getByTestId("profile-value-birthday")).toContainText("March");
  await expect(page.getByTestId("profile-value-birthday")).not.toContainText("1990");

  // Ordinary self editing and hide controls operate through the web UI.
  await memberPage.goto(`${instance.url}/people/${member.person.id}`);
  await expect(memberPage.getByTestId("profile-value-birthday")).toContainText("1990");
  const telegram = memberPage
    .getByRole("listitem")
    .filter({ has: memberPage.getByRole("heading", { name: "Telegram", exact: true }) });
  await telegram.getByRole("button", { name: "Edit", exact: true }).click();
  await telegram.getByLabel("Telegram", { exact: true }).fill("priya_updated");
  await telegram.getByRole("button", { name: "Save", exact: true }).click();
  await expect(telegram.getByTestId("profile-value-telegram")).toHaveText("priya_updated");

  const viewerContext = await browser.newContext();
  const viewerPage = await viewerContext.newPage();
  await signIn(viewerPage, instance.url, "jonas.lindqvist");
  await viewerPage.goto(`${instance.url}/people/${member.person.id}`);
  await expect(viewerPage.getByTestId("profile-value-telegram")).toHaveText("priya_updated");
  await expect(viewerPage.getByTestId("profile-value-birthday")).toContainText("March");
  await expect(viewerPage.getByText("1990", { exact: false })).toHaveCount(0);
  await expect(viewerPage.getByTestId("profile-value-office")).toHaveCount(0);
  await expect(viewerPage.getByRole("button", { name: "Edit", exact: true })).toHaveCount(0);
  const profile = await viewerPage.request.get(
    `${instance.url}/api/v1/people/${member.person.id}/profile`,
  );
  const body = await profile.json();
  expect(body.fields.map((e: { field: { key: string } }) => e.field.key)).toEqual([
    "telegram",
    "birthday",
  ]);
  expect(
    body.fields.find((e: { field: { key: string } }) => e.field.key === "birthday").value,
  ).toBe("--03-14");
  await viewerPage.screenshot({
    path: test.info().outputPath("member-profile.png"),
    fullPage: true,
    animations: "disabled",
  });
  await telegram.getByLabel("Hide from others").check();
  await expect(telegram.getByText("Hidden from others", { exact: true })).toBeVisible();
  await viewerPage.reload();
  await expect(viewerPage.getByTestId("profile-value-telegram")).toHaveText("—");
  // Disabling further hiding must still let the subject show an existing hidden value.
  await page.goto(`${instance.url}/settings/profile-fields`);
  const telegramSettings = page
    .getByRole("listitem")
    .filter({ has: page.getByRole("heading", { name: "Telegram", exact: true }) });
  await telegramSettings.getByText("Settings", { exact: true }).click();
  await telegramSettings.getByText("Advanced settings", { exact: true }).click();
  await telegramSettings.getByLabel("People can hide their value").uncheck();
  await telegramSettings.getByRole("button", { name: "Save", exact: true }).click();
  await expect(telegramSettings.getByRole("status")).toContainText("Saved");
  await page.goto(`${instance.url}/people/${member.person.id}`);
  await expect(page.getByLabel("Hide from others")).toHaveCount(0);
  await memberPage.reload();
  const unhide = memberPage
    .getByRole("listitem")
    .filter({ has: memberPage.getByRole("heading", { name: "Telegram", exact: true }) })
    .getByLabel("Hide from others");
  await expect(unhide).toBeChecked();
  await unhide.uncheck();
  await expect(memberPage.getByText("Hidden from others", { exact: true })).toHaveCount(0);
  await expect(unhide).toHaveCount(0);
  await viewerPage.reload();
  await expect(viewerPage.getByTestId("profile-value-telegram")).toHaveText("priya_updated");
  await viewerPage.goto(`${instance.url}/settings/profile-fields`);
  await expect(viewerPage.getByRole("alert")).toContainText("permission to manage people");
  await Promise.all([ownerContext.close(), memberContext.close(), viewerContext.close()]);
});
