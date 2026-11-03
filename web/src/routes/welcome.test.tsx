import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { me, renderApp, replies } from "@/test/harness";

const steps = [
  {
    id: "profile",
    title: "Profile",
    description: "",
    required: true,
    complete: true,
    actionUrl: `/people/${me.person.id}`,
  },
  {
    id: "avatar",
    title: "Photo",
    description: "",
    required: false,
    complete: false,
    actionUrl: `/people/${me.person.id}`,
  },
  {
    id: "passkey",
    title: "Passkey",
    description: "",
    required: false,
    complete: false,
    actionUrl: "/settings/security",
  },
];

describe("welcome", () => {
  it("leaves optional steps incomplete when finishing and returns to Home", async () => {
    let finished = false;
    const { user, called, history } = renderApp("/welcome", {
      "GET /api/v1/me": () => ({ status: 200, body: { ...me, welcomePending: !finished } }),
      "GET /api/v1/me/onboarding": () => ({
        status: 200,
        body: { personId: me.person.id, welcomePending: !finished, steps },
      }),
      "POST /api/v1/me/onboarding/finish": () => {
        finished = true;
        return { status: 204 };
      },
    });
    await user.click(
      await screen.findByRole("button", { name: "Skip optional steps and continue" }),
    );
    await waitFor(() => expect(history.location.pathname).toBe("/"));
    expect(await screen.findByTestId("onboarding-passkey")).toHaveTextContent("To do");
    expect(called("POST", "/api/v1/me/onboarding/finish")[0]?.headers.get("X-CSRF-Token")).toBe(
      me.csrfToken,
    );
  });
  it("keeps finish disabled while required profile is missing and exposes the authorized action", async () => {
    renderApp("/welcome", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/onboarding": {
        status: 200,
        body: {
          personId: me.person.id,
          welcomePending: true,
          steps: [{ ...steps[0], complete: false }],
        },
      },
      [`GET /api/v1/people/${me.person.id}/profile`]: {
        status: 200,
        body: { personId: me.person.id, fields: [] },
      },
    });
    expect(await screen.findByRole("button", { name: "Go to Home" })).toBeDisabled();
    expect(screen.getByRole("link", { name: "Open step: Complete your profile" })).toHaveAttribute(
      "href",
      `/people/${me.person.id}`,
    );
  });
  it("guards welcome when the session is absent", async () => {
    const { history, called } = renderApp("/welcome", { "GET /api/v1/me": replies.unauthorized });
    await screen.findByRole("heading", { name: /^Sign in$/ });
    expect(history.location.pathname).toBe("/sign-in");
    expect(called("GET", "/api/v1/me/onboarding")).toHaveLength(0);
  });
});
