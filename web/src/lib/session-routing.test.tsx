import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { browserNavigation } from "@/lib/navigation";
import { SessionGuard } from "@/lib/session-guard";
import * as webauthn from "@/lib/webauthn";
import { me, renderApp, replies, signInResult } from "@/test/harness";

function invitedSession(initiallySignedIn = false, passkeyOffer = false) {
  let valid = initiallySignedIn;
  let finished = false;
  let offer = passkeyOffer;
  return {
    expire: () => {
      valid = false;
    },
    routes: {
      "GET /api/v1/me": () =>
        valid
          ? {
              status: 200,
              body: { ...me, welcomePending: !finished, passkeyOffer: offer },
            }
          : replies.unauthorized,
      "POST /api/v1/auth/sign-in": () => {
        valid = true;
        return { status: 200, body: { ...signInResult, passkeyOffer: offer } };
      },
      "GET /api/v1/me/onboarding": () =>
        valid
          ? {
              status: 200,
              body: {
                personId: me.person.id,
                welcomePending: !finished,
                steps: [
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
                    title: "Avatar",
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
                ],
              },
            }
          : replies.unauthorized,
      "POST /api/v1/me/onboarding/finish": () => {
        finished = true;
        return { status: 204 };
      },
      "POST /api/v1/me/passkey-offer/dismiss": () => {
        offer = false;
        return { status: 204 };
      },
      "GET /api/v1/teams": { status: 200, body: { items: [] } },
    },
  };
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
  focusManager.setFocused(undefined);
});

describe("welcome sign-in continuation", () => {
  it("retains the validated Teams destination through concurrent expiry, sign-in and welcome completion", async () => {
    const end = vi.spyOn(SessionGuard.prototype, "end");
    const session = invitedSession(true);
    const { user, history } = renderApp("/welcome?redirect=%2Fteams", session.routes);
    await screen.findByRole("heading", { name: "Welcome to your workspace" });
    session.expire();
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 10 * 60_000);
    await act(async () => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await screen.findByRole("heading", { name: "Sign in" });
    expect(new URLSearchParams(history.location.search).get("redirect")).toBe(
      "/welcome?redirect=%2Fteams",
    );
    await user.type(screen.getByLabelText("Work email"), me.person.workEmail);
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Welcome to your workspace" });
    expect(new URLSearchParams(history.location.search).get("redirect")).toBe("/teams");
    await user.click(screen.getByRole("button", { name: "Skip optional steps and continue" }));
    await screen.findByRole("heading", { name: "Teams" });
    expect(history.location.pathname).toBe("/teams");
    expect(end).toHaveBeenCalledOnce();
  });

  it("keeps the same destination through the optional passkey offer and Skip", async () => {
    vi.spyOn(webauthn, "passkeysSupported").mockReturnValue(true);
    const session = invitedSession(false, true);
    const target = "/welcome?redirect=%2Fteams";
    const { user, history } = renderApp(
      `/sign-in?redirect=${encodeURIComponent(target)}`,
      session.routes,
    );
    await user.type(await screen.findByLabelText("Work email"), me.person.workEmail);
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Sign in faster with a passkey" });
    expect(new URLSearchParams(history.location.search).get("redirect")).toBe(target);
    await user.click(screen.getByRole("button", { name: "Skip for now" }));
    await screen.findByRole("heading", { name: "Welcome to your workspace" });
    expect(new URLSearchParams(history.location.search).get("redirect")).toBe("/teams");
    await user.click(screen.getByRole("button", { name: "Skip optional steps and continue" }));
    await screen.findByRole("heading", { name: "Teams" });
    expect(history.location.pathname).toBe("/teams");
  });

  it("flattens nested welcome links when already signed in", async () => {
    const session = invitedSession(true);
    const { user, history } = renderApp(
      `/welcome?redirect=${encodeURIComponent("/welcome?redirect=%2Fteams")}`,
      session.routes,
    );
    await user.click(
      await screen.findByRole("button", { name: "Skip optional steps and continue" }),
    );
    await screen.findByRole("heading", { name: "Teams" });
    expect(history.location.pathname).toBe("/teams");
  });

  it.each([
    "/welcome?redirect=https%3A%2F%2Fevil.example",
    "/welcome?redirect=%2F%2Fevil.example",
    "/welcome?redirect=%2Fwelcome",
  ])("does not follow unsafe or looping continuation %s", async (target) => {
    const assign = vi.spyOn(browserNavigation, "assign").mockImplementation(() => {});
    const session = invitedSession();
    const { user, history } = renderApp(
      `/sign-in?redirect=${encodeURIComponent(target)}`,
      session.routes,
    );
    await user.type(await screen.findByLabelText("Work email"), me.person.workEmail);
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Welcome to your workspace" });
    expect(history.location.pathname).toBe("/welcome");
    expect(new URLSearchParams(history.location.search).get("redirect")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Skip optional steps and continue" }));
    await waitFor(() => expect(history.location.pathname).toBe("/"));
    expect(assign).not.toHaveBeenCalled();
  });
});
