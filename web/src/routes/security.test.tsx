import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp } from "@/test/harness";

const other = {
  ...me.session,
  id: "0190a1b2-0000-7000-8000-0000000000bb",
  device: { id: "0190a1b2-0000-7000-8000-0000000000ee", name: "Safari on iPhone", platform: "ios" },
  authMethods: ["passkey"],
  current: false,
};

describe("sign-in & security", () => {
  it("lists sessions and signs another one out", async () => {
    let sessions = [me.session, other];
    const { user, called } = renderApp("/settings/security", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/passkeys": { status: 200, body: { items: [] } },
      "GET /api/v1/me/sessions": () => ({ status: 200, body: { items: sessions } }),
      [`DELETE /api/v1/me/sessions/${other.id}`]: () => {
        sessions = [me.session];
        return { status: 204 };
      },
    });
    expect(await screen.findByText("Safari on iPhone")).toBeInTheDocument();
    const current = screen.getByText("Chrome on macOS").closest("li");
    if (!current) throw new Error("no row");
    expect(within(current).getByText("This device")).toBeInTheDocument();
    expect(screen.getByText(/with a passkey/)).toBeInTheDocument();

    await user.click(
      screen.getByRole("button", { name: /^Sign out of Safari on iPhone, signed in / }),
    );
    expect(called("DELETE", `/api/v1/me/sessions/${other.id}`)).toHaveLength(1);
    expect(await screen.findByText("Chrome on macOS")).toBeInTheDocument();
    expect(screen.queryByText("Safari on iPhone")).not.toBeInTheDocument();
  });

  it("tells sessions of the same browser apart by time", async () => {
    const earlier = {
      ...me.session,
      id: "0190a1b2-0000-7000-8000-0000000000ff",
      current: false,
      createdAt: "2026-10-28T08:15:00Z",
      lastSeenAt: "2026-10-29T17:40:00Z",
    };
    renderApp("/settings/security", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/passkeys": { status: 200, body: { items: [] } },
      "GET /api/v1/me/sessions": { status: 200, body: { items: [earlier, me.session] } },
    });
    const buttons = await screen.findAllByRole("button", {
      name: /^Sign out of Chrome on macOS, signed in /,
    });
    expect(buttons).toHaveLength(2);
    // Accessible names differ, and each row shows the sign-in time to the minute.
    expect(new Set(buttons.map((b) => b.getAttribute("aria-label"))).size).toBe(2);
    const signedIn = document.querySelectorAll(`time[datetime="${earlier.createdAt}"]`);
    expect(signedIn).toHaveLength(1);
    expect(signedIn[0]?.textContent).toMatch(/\d{1,2}:15/);
    // Last activity is relative, with the exact time on hover.
    const lastActive = document.querySelector(`time[datetime="${earlier.lastSeenAt}"]`);
    expect(lastActive?.getAttribute("title")).toMatch(/\d{1,2}:40/);
    // The current session is first and marked.
    const rows = screen.getAllByRole("listitem");
    expect(
      within(rows.find((r) => r.textContent?.includes("Chrome on macOS")) as HTMLElement).getByText(
        "This device",
      ),
    ).toBeInTheDocument();
  });

  it("removes a passkey after confirmation", async () => {
    let passkeys = [
      {
        id: "0190a1b2-0000-7000-8000-0000000000cc",
        name: "Mac",
        createdAt: "2026-11-01T10:00:00Z",
      },
    ];
    const { user, called } = renderApp("/settings/security", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/sessions": { status: 200, body: { items: [me.session] } },
      "GET /api/v1/me/passkeys": () => ({ status: 200, body: { items: passkeys } }),
      "DELETE /api/v1/me/passkeys/0190a1b2-0000-7000-8000-0000000000cc": () => {
        passkeys = [];
        return { status: 204 };
      },
    });
    await user.click(await screen.findByRole("button", { name: "Remove passkey Mac" }));
    expect(
      called("DELETE", "/api/v1/me/passkeys/0190a1b2-0000-7000-8000-0000000000cc"),
    ).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(/No passkeys yet|doesn't support passkeys/)).toBeInTheDocument();
  });
});
