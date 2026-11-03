import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp, replies, signInResult } from "@/test/harness";

const invitation = {
  id: "0190a1b2-0000-7000-8000-000000000002",
  personId: "0190a1b2-0000-7000-8000-000000000003",
  displayName: "Maya Okafor",
  username: "maya",
  contactEmail: "maya@example.test",
  workEmail: "maya@acme.com",
  roles: ["member"],
  status: "pending",
  delivery: "email",
  issuedAt: "2026-11-03T09:00:00Z",
  createdAt: "2026-11-03T09:00:00Z",
  expiresAt: "2026-11-10T09:00:00Z",
};
const admin = { ...me, permissions: ["people.manage", "instance.manage"] };

describe("invitations", () => {
  it("explains the copy-link channel when selecting an additional permission grant", async () => {
    const { user } = renderApp("/people/invitations", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/invitations/options": {
        status: 200,
        body: {
          delivery: "email",
          workEmailPattern: "{username}@acme.com",
          lifetimeHours: 168,
          roles: ["member", "admin"],
          roleDeliveries: { member: "email", admin: "link" },
        },
      },
      "GET /api/v1/teams": { status: 200, body: { items: [] } },
      "GET /api/v1/invitations": { status: 200, body: { items: [] } },
    });
    await screen.findByRole("option", { name: "Admin" });
    await user.selectOptions(screen.getByLabelText("Role"), "admin");
    expect(await screen.findByText(/Share this invitation link directly/)).toBeVisible();
    await user.selectOptions(screen.getByLabelText("Role"), "member");
    expect(await screen.findByText(/Invitations are emailed to the contact address/)).toBeVisible();
  });
  it("includes selected initial teams in the invitation request", async () => {
    const teamID = "0190a1b2-0000-7000-8000-000000000099";
    const { user, called } = renderApp("/people/invitations", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/invitations/options": {
        status: 200,
        body: {
          delivery: "link",
          workEmailPattern: "{username}@acme.com",
          lifetimeHours: 168,
          roles: ["member", "admin"],
          roleDeliveries: { member: "email", admin: "link" },
        },
      },
      "GET /api/v1/teams": {
        status: 200,
        body: { items: [{ id: teamID, name: "Platform", slug: "platform", memberCount: 0 }] },
      },
      "GET /api/v1/invitations": { status: 200, body: { items: [] } },
      "POST /api/v1/invitations": {
        status: 201,
        body: { invitation, link: "https://work.acme.com/invite#oli_team" },
      },
    });
    await user.type(await screen.findByLabelText("Full name"), "Maya Okafor");
    await user.type(screen.getByLabelText("Contact email"), "maya@example.test");
    await user.type(screen.getByLabelText("Username"), "maya");
    await user.click(await screen.findByRole("checkbox", { name: "Platform" }));
    await user.click(screen.getByRole("button", { name: "Invite person" }));
    await screen.findByLabelText("Invitation link");
    expect(called("POST", "/api/v1/invitations")[0]?.body).toMatchObject({
      teamIds: [teamID],
      workEmail: "maya@acme.com",
      role: "member",
    });
  });
  it("hides management from members without calling the admin API", async () => {
    const { called } = renderApp("/people/invitations", {
      "GET /api/v1/me": { status: 200, body: me },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("don't have permission");
    expect(called("GET", "/api/v1/invitations")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Invite person" })).not.toBeInTheDocument();
  });

  it("shows SMTP errors and replaces a link on resend", async () => {
    const { user, called } = renderApp("/people/invitations", {
      "GET /api/v1/teams": { status: 200, body: { items: [] } },
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/invitations/options": {
        status: 200,
        body: {
          delivery: "email",
          workEmailPattern: "{username}@acme.com",
          lifetimeHours: 168,
          roles: ["member", "admin"],
          roleDeliveries: { member: "email", admin: "link" },
        },
      },
      "GET /api/v1/invitations": {
        status: 200,
        body: {
          items: [
            {
              ...invitation,
              email: {
                status: "failed",
                attempts: 6,
                maxAttempts: 6,
                lastError: "SMTP connection refused",
              },
            },
          ],
        },
      },
      [`POST /api/v1/invitations/${invitation.id}/resend`]: {
        status: 200,
        body: { invitation, link: "https://work.acme.com/invite#oli_new" },
      },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("SMTP connection refused");
    await user.click(screen.getByRole("button", { name: "Resend" }));
    expect(await screen.findByLabelText("Invitation link")).toHaveValue(
      "https://work.acme.com/invite#oli_new",
    );
    expect(
      called("POST", `/api/v1/invitations/${invitation.id}/resend`)[0]?.headers.get("X-CSRF-Token"),
    ).toBe(me.csrfToken);
  });

  it("previews a token from the fragment and accepts it in the request body", async () => {
    let accepted = false;
    const { user, called, history } = renderApp("/invite#oli_private", {
      "GET /api/v1/me": () => (accepted ? { status: 200, body: me } : replies.unauthorized),
      "POST /api/v1/auth/invitation": {
        status: 200,
        body: {
          displayName: "Alex Morgan",
          username: "alex",
          workEmail: "alex@acme.com",
          expiresAt: invitation.expiresAt,
        },
      },
      "POST /api/v1/auth/invitation/accept": () => {
        accepted = true;
        return { status: 200, body: signInResult };
      },
    });
    await screen.findByText("Welcome, Alex Morgan. Set a password to get started.");
    await user.type(
      screen.getByLabelText("Password", { exact: true }),
      "correct horse battery staple",
    );
    await user.click(screen.getByRole("button", { name: "Accept invitation" }));
    await waitFor(() => expect(history.location.pathname).toBe("/"));
    expect(called("POST", "/api/v1/auth/invitation")[0]?.body).toEqual({ token: "oli_private" });
    expect(called("POST", "/api/v1/auth/invitation/accept")[0]?.body).toEqual({
      token: "oli_private",
      password: "correct horse battery staple",
    });
  });

  it("explains expired links and offers sign-in", async () => {
    renderApp("/invite#oli_expired", {
      "POST /api/v1/auth/invitation": {
        status: 410,
        body: { type: "about:blank", status: 410, title: "Gone" },
      },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("expired");
    expect(screen.getByRole("link", { name: "Go to sign-in" })).toHaveAttribute("href", "/sign-in");
  });
});
