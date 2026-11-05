import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import type { APIRequestContext } from "@playwright/test";
import { expect, test } from "./fixtures";

test.use({ demo: true, reducedMotion: "reduce" });

// The server talks to a fake GitHub.com (build tag githubtest) whose every
// request and response is validated against GitHub's OpenAPI description.
// The browser reaches the fake's authorization page through routing; no
// real GitHub organization is involved.

async function signIn(request: APIRequestContext, url: string, username: string) {
  const res = await request.post(`${url}/api/v1/demo/sign-in`, {
    data: { username },
    headers: { Origin: url },
  });
  expect(res.ok()).toBeTruthy();
}

test("an employee links their GitHub account from the onboarding step", async ({
  page,
  instance,
  browser,
}) => {
  // An admin connects the fake organization through the public API (manual
  // setup), and gives everyone access so the step applies.
  await signIn(page.request, instance.url, "lucas.ferreira");
  const app = await (await page.request.get(`${instance.githubFake}/_fake/app`)).json();
  const admin = await (await page.request.get(`${instance.url}/api/v1/me`)).json();
  const headers = { Origin: instance.url, "X-CSRF-Token": admin.csrfToken };
  const begin = await page.request.post(`${instance.url}/api/v1/apps/github/connect`, {
    headers,
    data: {
      organization: app.organization,
      key: "github",
      manual: {
        appId: app.app_id,
        clientId: app.client_id,
        privateKey: app.private_key,
        clientSecret: app.client_secret,
        webhookSecret: app.webhook_secret,
      },
    },
  });
  expect(begin.ok(), await begin.text()).toBeTruthy();
  const step = await begin.json();
  const state = new URL(step.url).searchParams.get("state");
  const done = await page.request.post(`${instance.url}/api/v1/apps/github/connect/complete`, {
    headers,
    data: { state, installationId: app.installation_id },
  });
  expect(done.ok(), await done.text()).toBeTruthy();
  const { instanceId } = await done.json();
  const policy = await page.request.patch(`${instance.url}/api/v1/apps/instances/${instanceId}`, {
    headers,
    data: { accessPolicy: "all_people" },
  });
  expect(policy.ok(), await policy.text()).toBeTruthy();

  // The employee's GitHub account (fake) approves the authorization.
  await page.request.post(`${instance.githubFake}/_fake/authorize-as`, {
    data: { id: 583231, login: "sofia-on-github" },
  });
  const context = await browser.newContext();
  try {
    const employee = await context.newPage();
    await signIn(employee.request, instance.url, "sofia.petrova");
    await employee.route("https://github.com/login/oauth/**", async (route) => {
      const target = new URL(route.request().url());
      const response = await route.fetch({
        url: `${instance.githubFake}${target.pathname}${target.search}`,
        maxRedirects: 0,
      });
      await route.fulfill({ response });
    });

    await employee.goto(`${instance.url}/welcome`);
    const stepItem = employee.getByTestId("onboarding-github.link-account");
    await expect(stepItem).toContainText("Link your GitHub account");
    await expect(stepItem).toContainText("To do · Optional");
    await stepItem.getByRole("link").click();

    await expect(
      employee.getByRole("heading", { level: 1, name: "Link your GitHub account" }),
    ).toBeVisible();
    await expect(employee.getByText("No GitHub account linked yet.")).toBeVisible();
    const evidence = process.env.ORGLANE_PR_ASSETS;
    if (evidence) {
      await mkdir(evidence, { recursive: true });
      await employee.screenshot({
        path: join(evidence, "github-account-step.png"),
        fullPage: true,
        animations: "disabled",
      });
    }
    await employee.getByRole("button", { name: "Link GitHub account" }).click();
    await expect(
      employee.getByText("Orglane now knows that @sofia-on-github is your GitHub account."),
    ).toBeVisible();
    // The one-time code and state are gone from the address.
    expect(new URL(employee.url()).search).toBe("");

    await employee.getByRole("link", { name: "Back to GitHub account" }).click();
    await expect(
      employee.getByText("Linked to the GitHub account @sofia-on-github."),
    ).toBeVisible();
    await employee.goto(`${instance.url}/welcome`);
    await expect(employee.getByTestId("onboarding-github.link-account")).toContainText(
      "Complete · Optional",
    );
  } finally {
    await context.close();
  }

  // The admin sees the verified link on the person page and could revoke it.
  const people = await (await page.request.get(`${instance.url}/api/v1/people?q=sofia`)).json();
  const sofia = people.items.find((p: { username: string }) => p.username === "sofia.petrova");
  await page.goto(`${instance.url}/people/${sofia.id}`);
  await expect(page.getByText("@sofia-on-github")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Revoke the link to @sofia-on-github" }),
  ).toBeVisible();
});
