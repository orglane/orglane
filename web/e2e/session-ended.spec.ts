import { expect, owner, setUp, test } from "./fixtures";

// The session of an open page is revoked from elsewhere; when the person
// returns to the tab, the refetch of the (stale) current person finds no
// session, and the app takes them to sign-in and back.
test("a session revoked elsewhere returns the open page to sign-in", async ({
  page,
  instance,
  playwright,
}) => {
  await page.clock.install();
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await expect(page).toHaveURL(`${instance.url}/`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Inbox" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();

  // Another browser signs in as the same person and revokes this session.
  const other = await playwright.request.newContext({
    baseURL: instance.url,
    extraHTTPHeaders: { Origin: instance.url },
  });
  const signIn = await other.post("/api/v1/auth/sign-in", {
    data: { email: owner.email, password: owner.password },
  });
  expect(signIn.ok()).toBe(true);
  const { csrfToken } = (await signIn.json()) as { csrfToken: string };
  const { items } = (await (await other.get("/api/v1/me/sessions")).json()) as {
    items: { id: string; current: boolean }[];
  };
  const victim = items.find((s) => !s.current);
  expect(victim).toBeTruthy();
  const revoked = await other.delete(`/api/v1/me/sessions/${victim?.id}`, {
    headers: { "X-CSRF-Token": csrfToken },
  });
  expect(revoked.status()).toBe(204);
  await other.dispose();

  // Time passes and the person comes back to the tab.
  await page.clock.fastForward("10:00");
  await page.evaluate(() => window.dispatchEvent(new Event("visibilitychange")));

  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await expect(page.getByRole("status")).toContainText("Your session has ended.");
  await expect(page).toHaveURL(/\/sign-in\?.*redirect=%2Finbox/);
  await expect(page.getByText("Something went wrong")).toHaveCount(0);

  await page.getByLabel("Work email").fill(owner.email);
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(`${instance.url}/inbox`);
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
});
