import { createHash, randomBytes } from "node:crypto";

import { expect, owner, setUp, test } from "./fixtures";

// An application signs in through Orglane (OIDC authorization code + PKCE):
// /oidc/authorize hands the browser to the web UI's sign-in screen, which
// returns to the application once signed in.
test("sign in to an application through the sign-in screen", async ({ page, instance }) => {
  await setUp(page, instance);
  await page.getByRole("button", { name: "Skip for now" }).click();
  await page.context().clearCookies();

  const redirect = "https://app.example.test/callback";
  instance.cli(
    "oidc-client",
    "add",
    "-id",
    "e2e-app",
    "-name",
    "E2E App",
    "-public",
    "-redirect-uri",
    redirect,
  );

  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  const params = new URLSearchParams({
    client_id: "e2e-app",
    redirect_uri: redirect,
    response_type: "code",
    scope: "openid profile email",
    state: "s-1",
    code_challenge: challenge,
    code_challenge_method: "S256",
    login_hint: owner.email,
  });
  let callback = "";
  await page.route(`${redirect}**`, (route) => {
    callback = route.request().url();
    return route.fulfill({ status: 200, body: "back at the app" });
  });
  await page.goto(`${instance.url}/oidc/authorize?${params}`);
  await expect(page).toHaveURL(/\/sign-in\?authRequest=/);
  await expect(page.getByText("to continue to the application")).toBeVisible();
  await expect(page.getByLabel("Work email")).toHaveValue(owner.email);
  await page.getByLabel("Password", { exact: true }).fill(owner.password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByText("back at the app")).toBeVisible();
  const q = new URL(callback).searchParams;
  expect(q.get("state")).toBe("s-1");
  expect(q.get("code")).toBeTruthy();
});
