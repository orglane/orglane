import { authenticator, expect, owner, setUp, signOut, test } from "./fixtures";

// Passkey registration from the offer and sign-in with it, using Chromium's
// virtual authenticator.
test("add a passkey from the offer and sign in with it", async ({ page, instance, webauthn }) => {
  await setUp(page, instance);
  await expect(page.getByRole("heading", { name: "Sign in faster with a passkey" })).toBeVisible();
  await page.getByRole("button", { name: "Add a passkey" }).click();
  await expect(page.getByRole("heading", { name: "Passkey added" })).toBeVisible();
  const { credentials } = await webauthn.send("WebAuthn.getCredentials", {
    authenticatorId: authenticator(webauthn),
  });
  expect(credentials).toHaveLength(1);
  expect(credentials[0]?.isResidentCredential).toBe(true);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page).toHaveURL(`${instance.url}/`);

  // It shows in Sign-in & security.
  await page.goto(`${instance.url}/settings/security`);
  await expect(page.getByRole("button", { name: /^Remove passkey / })).toBeVisible();

  await signOut(page);
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL(`${instance.url}/`);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    owner.name.split(" ")[0] ?? "",
  );

  // The new session says how it signed in.
  await page.goto(`${instance.url}/settings/security`);
  await expect(page.getByText(/with a passkey/).first()).toBeVisible();
});
