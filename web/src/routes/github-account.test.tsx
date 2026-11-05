import { screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { me, renderApp } from "@/test/harness";

const linkable = [
  { provider: "github", instanceId: "0190a1b2-0000-7000-8000-0000000000a1", displayName: "GitHub" },
];
const linksPath = "/api/v1/me/account-links";

afterEach(() => {
  window.history.replaceState(null, "", "/");
  vi.restoreAllMocks();
});

describe("Link your GitHub account", () => {
  it("shows the expected account as a hint and sends the browser to GitHub", async () => {
    const assign = vi.fn();
    vi.spyOn(window, "location", "get").mockReturnValue({
      ...window.location,
      assign,
    } as Location);
    const app = renderApp("/me/accounts/github", {
      "GET /api/v1/me": { status: 200, body: me },
      [`GET ${linksPath}`]: {
        status: 200,
        body: {
          links: [
            {
              provider: "github",
              login: "octo-expected",
              externalId: "42",
              state: "pending",
              organizationId: 9001,
              conflict: false,
            },
          ],
          linkable,
        },
      },
      [`POST ${linksPath}/github/start`]: {
        status: 200,
        body: { url: "https://github.com/login/oauth/authorize?client_id=x" },
      },
    });
    expect(await screen.findByText(/expects the GitHub account @octo-expected/)).toBeVisible();
    expect(screen.getByText(/this hint proves nothing/)).toBeVisible();
    await app.user.click(screen.getByRole("button", { name: "Link GitHub account" }));
    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("https://github.com/login/oauth/authorize?client_id=x"),
    );
  });

  it("explains when GitHub is not connected", async () => {
    renderApp("/me/accounts/github", {
      "GET /api/v1/me": { status: 200, body: me },
      [`GET ${linksPath}`]: { status: 200, body: { links: [], linkable: [] } },
    });
    expect(await screen.findByText(/has not connected GitHub to Orglane yet/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Link GitHub account" })).not.toBeInTheDocument();
  });

  it("unlinks the account after a confirmation", async () => {
    const linkId = "0190a1b2-0000-7000-8000-0000000000c1";
    const app = renderApp("/me/accounts/github", {
      "GET /api/v1/me": { status: 200, body: me },
      [`GET ${linksPath}`]: {
        status: 200,
        body: {
          links: [
            {
              id: linkId,
              provider: "github",
              realm: "github.com",
              login: "octo-me",
              externalId: "42",
              state: "verified",
              method: "oauth",
            },
          ],
          linkable,
        },
      },
      [`DELETE ${linksPath}/${linkId}`]: { status: 204 },
    });
    expect(await screen.findByText("Linked to the GitHub account @octo-me.")).toBeVisible();
    await app.user.click(screen.getByRole("button", { name: "Unlink" }));
    expect(screen.getByText(/Unlink @octo-me\?/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
    expect(app.called("DELETE", `${linksPath}/${linkId}`)).toHaveLength(0);
    await app.user.click(screen.getByRole("button", { name: "Unlink account" }));
    await waitFor(() => expect(app.called("DELETE", `${linksPath}/${linkId}`)).toHaveLength(1));
  });
});

describe("GitHub account callback", () => {
  const callback = (query: string) =>
    window.history.replaceState(null, "", `/me/accounts/github/callback?${query}`);

  it("completes once with state and code and removes them from the address", async () => {
    callback("state=oll_example&code=one-time");
    const app = renderApp("/me/accounts/github/callback", {
      "GET /api/v1/me": { status: 200, body: me },
      [`POST ${linksPath}/github/complete`]: {
        status: 200,
        body: {
          provider: "github",
          login: "octo-me",
          externalId: "42",
          state: "verified",
        },
      },
    });
    expect(
      await screen.findByText("Orglane now knows that @octo-me is your GitHub account."),
    ).toBeVisible();
    const calls = app.called("POST", `${linksPath}/github/complete`);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.body).toEqual({ state: "oll_example", code: "one-time" });
    expect(window.location.search).toBe("");
  });

  it("says nothing was linked when the person declines on GitHub", async () => {
    callback("error=access_denied&state=oll_example");
    const app = renderApp("/me/accounts/github/callback", {
      "GET /api/v1/me": { status: 200, body: me },
    });
    expect(await screen.findByText(/You did not authorize on GitHub/)).toBeVisible();
    expect(app.called("POST", `${linksPath}/github/complete`)).toHaveLength(0);
  });

  it.each([
    ["invalid_state", /expired or was started in another browser/],
    ["account_already_linked", /already linked to another person in Orglane/],
  ])("explains %s", async (code, text) => {
    callback("state=oll_example&code=one-time");
    renderApp("/me/accounts/github/callback", {
      "GET /api/v1/me": { status: 200, body: me },
      [`POST ${linksPath}/github/complete`]: {
        status: 409,
        body: { type: "about:blank", title: "Conflict", status: 409, code },
      },
    });
    expect(await screen.findByText(text)).toBeVisible();
  });
});
