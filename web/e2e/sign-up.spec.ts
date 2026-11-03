import { expect, owner, setUp, signOut, test } from "./fixtures";

// The issue's acceptance flow: first-run setup → passkey offer → Skip →
// shell → sign out → sign in again.
test("setup, skip the passkey offer, use the shell, sign out and in", async ({
  page,
  instance,
  webauthn: _,
}) => {
  await setUp(page, instance);

  // Passkeys are supported (virtual authenticator), so the offer comes first.
  await expect(page).toHaveURL(/\/welcome\/passkey$/);
  await expect(page.getByRole("heading", { name: "Sign in faster with a passkey" })).toBeVisible();
  await page.getByRole("button", { name: "Skip for now" }).click();

  await expect(page).toHaveURL(`${instance.url}/`);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Alex");
  const nav = page.getByRole("navigation", { name: "Main" });
  for (const [name, path, heading] of [
    ["Inbox", "/inbox", "Inbox"],
    ["People", "/people", "People"],
    ["Apps", "/apps", "Apps"],
  ] as const) {
    await nav.getByRole("link", { name }).click();
    await expect(page).toHaveURL(`${instance.url}${path}`);
    await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
    await expect(nav.getByRole("link", { name })).toHaveAttribute("aria-current", "page");
  }

  // Reloading a deep link works (SPA fallback) and keeps the session.
  await page.reload();
  await expect(page.getByRole("heading", { level: 1, name: "Apps" })).toBeVisible();

  // The current session is listed.
  await page.getByRole("button", { name: `Account menu for ${owner.name}` }).click();
  await page.getByRole("menuitem", { name: "Sign-in & security" }).click();
  await expect(page.getByText("This device")).toBeVisible();

  await signOut(page);
  await expect(page).toHaveURL(/\/sign-in/);

  // Signed out: the app sends you to sign-in, and back afterwards.
  await page.goto(`${instance.url}/people`);
  await expect(page).toHaveURL(/\/sign-in\?redirect=%2Fpeople$/);
  await page.getByLabel("Work email").fill(owner.email);
  await page.getByLabel("Password", { exact: true }).fill("not the password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText("The email or password is incorrect.");
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  // The offer was skipped, so it does not come back.
  await expect(page).toHaveURL(`${instance.url}/people`);
  await expect(page.getByRole("heading", { level: 1, name: "People" })).toBeVisible();
});

test("the server sends security and caching headers", async ({ request, instance }) => {
  const index = await request.get(`${instance.url}/inbox`);
  expect(index.status()).toBe(200);
  expect(index.headers()["cache-control"]).toBe("no-cache");
  expect(index.headers()["content-security-policy"]).toContain("script-src 'self'");
  const html = await index.text();
  const script = /src="(\/assets\/[^"]+\.js)"/.exec(html)?.[1];
  expect(script).toBeTruthy();
  const asset = await request.get(`${instance.url}${script}`);
  expect(asset.headers()["cache-control"]).toBe("public, max-age=31536000, immutable");
  expect((await request.get(`${instance.url}/assets/missing.js`)).status()).toBe(404);
});

test("no request leaves the origin and the CSP is never violated", async ({ page, instance }) => {
  const foreign: string[] = [];
  const violations: string[] = [];
  page.on("request", (r) => {
    if (!r.url().startsWith(instance.url)) foreign.push(r.url());
  });
  page.on("console", (m) => {
    if (/Content Security Policy|Refused to/i.test(m.text())) violations.push(m.text());
  });
  await setUp(page, instance);
  await expect(page).toHaveURL(/\/welcome\/passkey$/);
  await page.goto(`${instance.url}/settings/security`);
  await expect(page.getByText("This device")).toBeVisible();
  await page.getByRole("button", { name: `Account menu for ${owner.name}` }).click();
  await page.getByRole("menuitem", { name: "Theme" }).hover();
  await page.getByRole("menuitemradio", { name: "Dark" }).click();
  await expect(page.locator("html")).toHaveClass(/dark/);
  expect(foreign).toEqual([]);
  expect(violations).toEqual([]);
});
