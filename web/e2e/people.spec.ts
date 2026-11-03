import { mkdirSync } from "node:fs";
import { repo } from "./env";
import { expect, test } from "./fixtures";

test.use({ demo: true });

test("directory search and filters, profile header, self/admin edits, manager and avatar", async ({
  page,
  browser,
  instance,
}) => {
  const signedIn = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  expect(signedIn.ok()).toBeTruthy();
  const admin = await signedIn.json();
  const headers = { Origin: instance.url, "X-CSRF-Token": admin.csrfToken };
  await page.goto(`${instance.url}/people`);
  await expect(page.getByRole("heading", { name: "People", exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Invite people", exact: true })).toHaveAttribute(
    "href",
    "/people/invitations",
  );
  await page.getByLabel("Search people").fill("priya.raman@");
  const priyaLink = page.getByRole("link", { name: /Priya Raman/ });
  await expect(priyaLink).toBeVisible();
  await expect(page.getByRole("link", { name: /Maya Okafor/ })).toHaveCount(0);
  await page.getByLabel("Role", { exact: true }).selectOption("owner");
  await expect(page.getByText("No people found", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await page.getByLabel("Team", { exact: true }).selectOption({ label: "Platform" });
  await expect(priyaLink).toBeVisible();
  await priyaLink.click();
  await expect(page.getByRole("heading", { level: 1, name: "Priya Raman" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Platform", exact: true })).toBeVisible();
  await expect(page.locator("main img")).toBeVisible(); // Existing demo SVGs remain valid.
  const profileUrl = page.url();
  const personId = profileUrl.split("/").at(-1);
  await page.getByRole("button", { name: "Edit profile" }).click();
  await page.getByLabel("Job title", { exact: true }).fill("Staff Robotics Engineer");
  await page.getByLabel("Search for a manager").fill("Maya");
  await page
    .getByLabel("Reports to", { exact: true })
    .selectOption({ label: "Maya Okafor (@maya.okafor)" });
  await page.getByRole("button", { name: "Save profile" }).click();
  await expect(page.getByText("Profile saved.", { exact: true })).toBeVisible();
  await expect(page.getByText("Staff Robotics Engineer", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Maya Okafor", exact: true })).toBeVisible();
  // Real decoded PNG, not a signature-only dummy.
  await page.getByLabel("Upload avatar").setInputFiles({
    name: "avatar.png",
    mimeType: "image/png",
    buffer: Buffer.from(
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNQaPjwHwAE5AKQBYzgxgAAAABJRU5ErkJggg==",
      "base64",
    ),
  });
  await expect(page.getByText("Avatar updated.", { exact: true })).toBeVisible();
  const stored = await page.request.get(`${instance.url}/api/v1/people/${personId}/avatar`);
  expect(stored.headers()["content-type"]).toContain("image/png");

  const context = await browser.newContext();
  try {
    const self = await context.newPage();
    const login = await context.request.post(`${instance.url}/api/v1/demo/sign-in`, {
      data: { username: "priya.raman" },
      headers: { Origin: instance.url },
    });
    expect(login.ok()).toBeTruthy();
    await self.goto(profileUrl);
    await self.getByRole("button", { name: "Edit profile" }).click();
    await expect(self.getByLabel("Username", { exact: true })).toHaveCount(0);
    await expect(self.getByLabel("Reports to", { exact: true })).toHaveCount(0);
    await self.getByLabel("Job title", { exact: true }).fill("Senior Robotics Engineer");
    await self.getByRole("button", { name: "Save profile" }).click();
    await expect(self.getByText("Profile saved.", { exact: true })).toBeVisible();
    const me = await (await context.request.get(`${instance.url}/api/v1/me`)).json();
    const denied = await context.request.patch(`${instance.url}/api/v1/people/${personId}`, {
      headers: { Origin: instance.url, "X-CSRF-Token": me.csrfToken },
      data: { managerId: "" },
    });
    expect(denied.status()).toBe(403);
    await self.getByRole("link", { name: "Maya Okafor", exact: true }).click();
    await expect(self.getByRole("heading", { level: 1, name: "Maya Okafor" })).toBeVisible();
    await expect(self.getByRole("button", { name: "Edit profile" })).toHaveCount(0);
    await expect(self.getByLabel("Upload avatar")).toHaveCount(0);
  } finally {
    await context.close();
  }

  // Ready-made public profile fields use the existing central service/UI.
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/profile-field-library/telegram`, { headers })
    ).ok(),
  ).toBeTruthy();
  expect(
    (
      await page.request.patch(`${instance.url}/api/v1/people/${personId}/profile`, {
        headers,
        data: { values: [{ key: "telegram", value: "priya_raman" }] },
      })
    ).ok(),
  ).toBeTruthy();
  await page.reload();
  await expect(page.getByTestId("profile-value-telegram")).toHaveText("priya_raman");
});

test("demo directory and profile screenshots at 1440, 768 and 360 in both themes", async ({
  page,
  instance,
}) => {
  test.setTimeout(120_000);
  const login = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  const admin = await login.json();
  const headers = { Origin: instance.url, "X-CSRF-Token": admin.csrfToken };
  const people = await (await page.request.get(`${instance.url}/api/v1/people`)).json();
  const priya = people.items.find((p: { username: string }) => p.username === "priya.raman");
  const maya = people.items.find((p: { username: string }) => p.username === "maya.okafor");
  expect(
    (
      await page.request.patch(`${instance.url}/api/v1/people/${priya.id}`, {
        headers,
        data: { managerId: maya.id },
      })
    ).ok(),
  ).toBeTruthy();
  expect(
    (
      await page.request.post(`${instance.url}/api/v1/profile-field-library/telegram`, { headers })
    ).ok(),
  ).toBeTruthy();
  expect(
    (
      await page.request.patch(`${instance.url}/api/v1/people/${priya.id}/profile`, {
        headers,
        data: { values: [{ key: "telegram", value: "priya_raman" }] },
      })
    ).ok(),
  ).toBeTruthy();
  const directory = `${repo}.github/pr-assets/directory`;
  mkdirSync(directory, { recursive: true });
  for (const width of [1440, 768, 360]) {
    await page.setViewportSize({ width, height: width === 360 ? 900 : 1000 });
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      for (const [name, route] of [
        ["directory", "/people"],
        ["profile", `/people/${priya.id}`],
      ] as const) {
        await page.goto(`${instance.url}${route}`);
        await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${theme}\\b`));
        if (name === "directory") {
          await expect(page.getByRole("link", { name: /Priya Raman/ })).toBeVisible();
        } else {
          await expect(page.getByTestId("profile-value-telegram")).toHaveText("priya_raman");
        }
        await expect
          .poll(() =>
            page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
          )
          .toBe(true);
        await page.screenshot({
          path: `${directory}/${name}-${width}-${theme}.png`,
          fullPage: true,
          animations: "disabled",
        });
      }
    }
  }
});
