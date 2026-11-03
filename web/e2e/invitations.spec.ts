import { expect, owner, setUp, signOut, test } from "./fixtures";

test("invite without email, copy a link, accept and skip the passkey offer", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/teams`);
  await page.getByRole("button", { name: "New team", exact: true }).click();
  await page.getByLabel("Team name").fill("Platform");
  await page.getByLabel("Slug", { exact: true }).fill("platform");
  await page.getByRole("button", { name: "Create team", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Platform", exact: true })).toBeVisible();
  await page.goto(`${instance.url}/people`);
  await page.getByRole("link", { name: "Invite people" }).click();
  await expect(page.getByText("System email is not configured.", { exact: false })).toBeVisible();
  await page.getByLabel("Full name").fill("Maya Okafor");
  await page.getByLabel("Contact email").fill("maya@example.test");
  await page.getByLabel("Username").fill("maya");
  await expect(page.getByLabel("Work email")).toHaveValue("maya@localhost");
  await page.getByRole("checkbox", { name: "Platform", exact: true }).check();
  await page.getByRole("button", { name: "Invite person", exact: true }).click();
  const link = await page.getByLabel("Invitation link").inputValue();
  expect(link).toMatch(/\/invite#oli_/);
  await signOut(page);
  await page.goto(link);
  await expect(page.getByText("Welcome, Maya Okafor.", { exact: false })).toBeVisible();
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Accept invitation", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in faster with a passkey" })).toBeVisible();
  await page.getByRole("button", { name: "Skip for now" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Maya");
  await page.goto(`${instance.url}/teams`);
  await page.getByRole("link", { name: /Platform/ }).click();
  await expect(page.getByText("Maya Okafor", { exact: true })).toBeVisible();
  await page.goto(link);
  await expect(page.getByRole("alert")).toContainText("already accepted");
});

test("configure SMTP, see delivery failure, fix it and accept the emailed link", async ({
  page,
  instance,
  mailpit,
  webauthn: _,
}) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.goto(`${instance.url}/settings/email`);
  await page.getByLabel("SMTP host").fill("127.0.0.1");
  await page.getByLabel("Connection security").selectOption("none");
  await page.getByLabel("SMTP port").fill("1");
  await page.getByLabel("From address").fill("Orglane <no-reply@acme.test>");
  await page.getByRole("button", { name: "Save email settings", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("Email settings saved.");
  await page.getByLabel("Test recipient").fill("owner@example.test");
  await page.getByRole("button", { name: "Send test email", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("SMTP server could not deliver");

  await page.goto(`${instance.url}/people/invitations`);
  await page.getByLabel("Full name").fill("Ravi Patel");
  await page.getByLabel("Contact email").fill("ravi@example.test");
  await page.getByLabel("Username").fill("ravi");
  await page.getByRole("button", { name: "Invite person", exact: true }).click();
  await expect(
    page.getByText("Email delivery failed. Check email settings and resend."),
  ).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole("alert")).toContainText("connect");

  await page.getByRole("link", { name: "Email settings", exact: true }).click();
  await page.getByLabel("SMTP port").fill(String(mailpit.smtpPort));
  await page.getByRole("button", { name: "Save email settings", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("Email settings saved.");
  await page.getByLabel("Test recipient").fill("owner@example.test");
  await page.getByRole("button", { name: "Send test email", exact: true }).click();
  await expect(
    page.getByText("Test email delivered to the SMTP server.", { exact: false }),
  ).toBeVisible();
  await page.goto(`${instance.url}/people/invitations`);
  await page.getByRole("button", { name: "Resend", exact: true }).click();
  await expect(page.getByText("Email sent", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Invitation link")).toHaveCount(0);
  let messageID = "";
  await expect
    .poll(async () => {
      const list = (await (
        await fetch(`${mailpit.api}/api/v1/search?query=to%3Aravi%40example.test`)
      ).json()) as { messages: { ID: string }[] };
      messageID = list.messages[0]?.ID ?? "";
      return messageID;
    })
    .not.toBe("");
  const message = (await (await fetch(`${mailpit.api}/api/v1/message/${messageID}`)).json()) as {
    Text: string;
    HTML: string;
  };
  const link = message.Text.match(/http:\/\/localhost:\d+\/invite#oli_[A-Za-z0-9_-]+/)?.[0];
  expect(link).toBeTruthy();
  expect(message.HTML).toContain(link);
  await signOut(page);
  await page.goto(link ?? "");
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Accept invitation", exact: true }).click();
  await page.getByRole("button", { name: "Skip for now" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Ravi");
});
