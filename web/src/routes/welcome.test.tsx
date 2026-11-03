import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
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

afterEach(() => focusManager.setFocused(undefined));

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
    const continueButton = await screen.findByRole("button", {
      name: "Skip optional steps and continue",
    });
    expect(continueButton).not.toHaveFocus();
    await user.click(continueButton);
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
  it("transfers keyboard focus after the last required save and keeps a polite completion region", async () => {
    let complete = false;
    const profile = () => ({
      personId: me.person.id,
      fields: [
        {
          field: {
            id: "0190a1b2-0000-7000-8000-000000000002",
            key: "office",
            label: "Office",
            library: false,
            type: "text",
            section: "custom",
            visibility: "everyone",
            options: [],
            requiredAtOnboarding: true,
            editableBy: "self",
            userCanHide: false,
            sensitive: false,
            position: 1,
            archived: false,
          },
          editable: true,
          hidden: false,
          hideable: false,
          yearHidden: false,
          ...(complete ? { value: "Lisbon" } : {}),
        },
      ],
    });
    const { user } = renderApp("/welcome", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/onboarding": () => ({
        status: 200,
        body: {
          personId: me.person.id,
          welcomePending: true,
          steps: steps.map((step) => (step.id === "profile" ? { ...step, complete } : step)),
        },
      }),
      [`GET /api/v1/people/${me.person.id}/profile`]: () => ({ status: 200, body: profile() }),
      [`PATCH /api/v1/people/${me.person.id}/profile`]: () => {
        complete = true;
        return { status: 200, body: profile() };
      },
    });
    const row = await screen.findByTestId("onboarding-profile");
    const status = within(row).getByRole("status", { name: "Complete your profile" });
    expect(status).toHaveTextContent("To do · Required");
    expect(status).toHaveAttribute("aria-live", "polite");
    expect(status).toHaveAttribute("aria-atomic", "true");
    await user.click(await screen.findByRole("button", { name: /^Edit$/ }));
    await user.type(screen.getByLabelText("Office"), "Lisbon");
    await user.tab();
    expect(screen.getByRole("button", { name: /^Save$/ })).toHaveFocus();
    await user.keyboard("{Enter}");
    const continueButton = screen.getByRole("button", { name: "Skip optional steps and continue" });
    await waitFor(() => expect(continueButton).toHaveFocus());
    expect(continueButton).toBeEnabled();
    expect(screen.queryByLabelText("Office")).not.toBeInTheDocument();
    expect(within(row).getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("Complete · Required");
  });

  it("announces background completion without moving focus", async () => {
    let complete = false;
    const { unmount } = renderApp("/welcome", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/onboarding": () => ({
        status: 200,
        body: {
          personId: me.person.id,
          welcomePending: true,
          steps: steps.map((step) => (step.id === "profile" ? { ...step, complete } : step)),
        },
      }),
      [`GET /api/v1/people/${me.person.id}/profile`]: {
        status: 200,
        body: { personId: me.person.id, fields: [] },
      },
    });
    const row = await screen.findByTestId("onboarding-profile");
    const status = within(row).getByRole("status");
    const link = screen.getByRole("link", { name: "Open step: Add a passkey" });
    link.focus();
    expect(link).toHaveFocus();
    complete = true;
    await act(async () => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await waitFor(() => expect(status).toHaveTextContent("Complete · Required"));
    expect(link).toHaveFocus();
    expect(within(row).getByRole("status")).toBe(status);
    unmount();
  });

  it("guards welcome when the session is absent", async () => {
    const { history, called } = renderApp("/welcome", { "GET /api/v1/me": replies.unauthorized });
    await screen.findByRole("heading", { name: /^Sign in$/ });
    expect(history.location.pathname).toBe("/sign-in");
    expect(called("GET", "/api/v1/me/onboarding")).toHaveLength(0);
  });
});
