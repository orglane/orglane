import { expect, test } from "./fixtures";

test.use({ demo: true });

test("an Admin creates a team, adds two people and sets a lead; a Member can only view", async ({
  page,
  instance,
  browser,
}) => {
  const signedIn = await page.request.post(`${instance.url}/api/v1/demo/sign-in`, {
    data: { username: "lucas.ferreira" },
    headers: { Origin: instance.url },
  });
  expect(signedIn.ok()).toBeTruthy();
  await page.goto(`${instance.url}/teams`);
  await expect(page.getByRole("heading", { name: "Teams", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "New team" }).click();
  await page.getByLabel("Team name").fill("Field Robotics");
  await page.getByLabel("Slug", { exact: true }).fill("field-robotics");
  await page.getByLabel("Description", { exact: true }).fill("Deploying robots with customers.");
  await page.getByRole("button", { name: "Create team" }).click();
  await expect(page.getByRole("heading", { name: "Field Robotics", exact: true })).toBeVisible();
  const teamUrl = page.url();
  await page.getByLabel("Person to add").selectOption({ label: "Priya Raman (@priya.raman)" });
  await page.getByRole("button", { name: "Add member", exact: true }).click();
  await expect(page.getByRole("button", { name: "Make Priya Raman a lead" })).toBeVisible();
  await page.getByLabel("Person to add").selectOption({ label: "Chen Wei (@chen.wei)" });
  await page.getByRole("button", { name: "Add member", exact: true }).click();
  await page.getByRole("button", { name: "Make Priya Raman a lead" }).click();
  const priya = page.getByRole("listitem").filter({ hasText: "Priya Raman" });
  await expect(priya.getByText("Lead", { exact: true })).toBeVisible();
  await expect(page.getByRole("listitem").filter({ hasText: "Chen Wei" })).toBeVisible();

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
    await member.goto(teamUrl);
    await expect(
      member.getByRole("heading", { name: "Field Robotics", exact: true }),
    ).toBeVisible();
    await expect(
      member
        .getByRole("listitem")
        .filter({ hasText: "Priya Raman" })
        .getByText("Lead", { exact: true }),
    ).toBeVisible();
    await expect(member.getByText("Chen Wei", { exact: true })).toBeVisible();
    await expect(member.getByRole("button", { name: "Edit team" })).toHaveCount(0);
    await expect(member.getByRole("button", { name: "Add member", exact: true })).toHaveCount(0);
    await expect(
      member.getByRole("button", {
        name: /Make .* a (lead|member)|Remove .* from team|Archive team/,
      }),
    ).toHaveCount(0);
    const me = await (await context.request.get(`${instance.url}/api/v1/me`)).json();
    const denied = await context.request.post(`${instance.url}/api/v1/teams`, {
      data: { name: "Denied team", slug: "denied-team" },
      headers: { Origin: instance.url, "X-CSRF-Token": me.csrfToken },
    });
    expect(denied.status()).toBe(403);
    await member.goto(`${instance.url}/teams`);
    await member.getByLabel("Search teams").fill("Field Robotics");
    await expect(member.getByRole("link", { name: /Field Robotics/ })).toContainText("2 members");
    await expect(member.getByRole("button", { name: "New team" })).toHaveCount(0);
  } finally {
    await context.close();
  }

  await page.getByRole("button", { name: "Remove Chen Wei from team" }).click();
  await expect(page.getByText("Chen Wei", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Edit team" }).click();
  await page.getByLabel("Team name").fill("Field Engineering");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("heading", { name: "Field Engineering", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Archive team" }).click();
  await page.getByRole("button", { name: "Confirm archive" }).click();
  await expect(
    page.getByText("This team is archived. Its members and history are kept."),
  ).toBeVisible();
  await expect(page.getByText("Priya Raman", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Edit team" })).toHaveCount(0);
  await page.getByRole("link", { name: "All teams" }).click();
  await page.getByLabel("Search teams").fill("Field Engineering");
  await expect(page.getByText("No teams found")).toBeVisible();
  await page.getByLabel("Include archived teams").check();
  await expect(page.getByRole("link", { name: /Field Engineering/ })).toContainText("Archived");
});
