import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp } from "@/test/harness";

const signedIn = {
  "GET /api/v1/me": { status: 200, body: me },
  "GET /api/v1/teams": { status: 200, body: { items: [] } },
};

describe("the app shell", () => {
  it("navigates between the main sections", async () => {
    const { user, history } = renderApp("/", signedIn);
    expect(await screen.findByRole("heading", { level: 1, name: /Alex/ })).toBeInTheDocument();
    const nav = screen.getAllByRole("navigation", { name: "Main" })[0];
    if (!nav) throw new Error("no navigation");
    for (const [name, path] of [
      ["Inbox", "/inbox"],
      ["People", "/people"],
      ["Teams", "/teams"],
      ["Apps", "/apps"],
      ["Home", "/"],
    ] as const) {
      await user.click(within(nav).getByRole("link", { name }));
      expect(history.location.pathname).toBe(path);
      expect(within(nav).getByRole("link", { name })).toHaveAttribute("aria-current", "page");
    }
    await user.click(within(nav).getByRole("link", { name: "Inbox" }));
    expect(await screen.findByText("You're all caught up")).toBeInTheDocument();
  });

  it("offers a skip link to the content", async () => {
    renderApp("/inbox", signedIn);
    expect(await screen.findByRole("link", { name: "Skip to content" })).toHaveAttribute(
      "href",
      "#main",
    );
  });

  it("switches the theme from the profile menu with the keyboard", async () => {
    const { user } = renderApp("/", signedIn);
    const [menu] = await screen.findAllByRole("button", { name: "Account menu for Alex Morgan" });
    if (!menu) throw new Error("no menu");
    menu.focus();
    await user.keyboard("{Enter}");
    const theme = await screen.findByRole("menuitem", { name: "Theme" });
    theme.focus();
    await user.keyboard("{ArrowRight}");
    const dark = await screen.findByRole("menuitemradio", { name: "Dark" });
    dark.focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.documentElement).toHaveClass("dark"));
    expect(localStorage.getItem("orglane.theme")).toBe("dark");
  });

  it("signs out", async () => {
    let signedOut = false;
    const { user, history, called } = renderApp("/", {
      "GET /api/v1/me": () =>
        signedOut
          ? { status: 401, body: { type: "about:blank", title: "Unauthorized", status: 401 } }
          : { status: 200, body: me },
      "POST /api/v1/auth/sign-out": () => {
        signedOut = true;
        return { status: 204 };
      },
    });
    const [menu] = await screen.findAllByRole("button", { name: "Account menu for Alex Morgan" });
    if (!menu) throw new Error("no menu");
    await user.click(menu);
    await user.click(await screen.findByRole("menuitem", { name: "Sign out" }));
    expect(await screen.findByText("You signed out.")).toBeInTheDocument();
    expect(history.location.pathname).toBe("/sign-in");
    expect(called("POST", "/api/v1/auth/sign-out")[0]?.headers.get("X-CSRF-Token")).toBe(
      "csrf-123",
    );
  });
});
