import { focusManager } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { browserNavigation } from "@/lib/navigation";
import { me, renderApp, replies, signInResult } from "@/test/harness";

afterEach(() => vi.restoreAllMocks());

describe("sign-in", () => {
  it("sends signed-out visitors of the app to sign-in, then back", async () => {
    let signedIn = false;
    const { user, history, called } = renderApp("/inbox", {
      "GET /api/v1/me": () => (signedIn ? { status: 200, body: me } : replies.unauthorized),
      "POST /api/v1/auth/sign-in": () => {
        signedIn = true;
        return { status: 200, body: signInResult };
      },
    });
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(history.location.pathname).toBe("/sign-in");

    await user.type(screen.getByLabelText("Work email"), "alex@acme.com");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument();
    expect(history.location.pathname).toBe("/inbox");
    expect(called("POST", "/api/v1/auth/sign-in")[0]?.body).toEqual({
      email: "alex@acme.com",
      password: "correct horse battery",
    });
  });

  it("explains a wrong password and clears the field", async () => {
    const { user } = renderApp("/sign-in", { "POST /api/v1/auth/sign-in": replies.unauthorized });
    await user.type(await screen.findByLabelText("Work email"), "alex@acme.com");
    await user.type(screen.getByLabelText("Password"), "wrong password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The email or password is incorrect.",
    );
    expect(screen.getByLabelText("Password")).toHaveValue("");
  });

  it("explains rate limiting with the wait time", async () => {
    const { user } = renderApp("/sign-in", { "POST /api/v1/auth/sign-in": replies.tooMany(300) });
    await user.type(await screen.findByLabelText("Work email"), "alex@acme.com");
    await user.type(screen.getByLabelText("Password"), "whatever-password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Too many attempts. Try again in 5 minutes.",
    );
  });

  it("returns to the OIDC flow after signing in for an application", async () => {
    const assign = vi.spyOn(browserNavigation, "assign").mockImplementation(() => {});
    const { user } = renderApp("/sign-in?authRequest=abc-123&email=alex%40acme.com", {
      "POST /api/v1/auth/sign-in": { status: 200, body: { ...signInResult, passkeyOffer: true } },
    });
    expect(await screen.findByText(/to continue to the application/)).toBeInTheDocument();
    expect(screen.getByLabelText("Work email")).toHaveValue("alex@acme.com");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/oidc/login?authRequest=abc-123"));
  });

  it("shows the password toggle with an accessible name", async () => {
    const { user } = renderApp("/sign-in");
    const toggle = await screen.findByRole("button", { name: "Show password" });
    await user.click(toggle);
    expect(screen.getByLabelText("Password")).toHaveAttribute("type", "text");
    expect(screen.getByRole("button", { name: "Hide password" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("goes to setup while the instance has no owner", async () => {
    const { history } = renderApp("/", {
      "GET /api/v1/auth/setup": { status: 200, body: { required: true } },
    });
    expect(
      await screen.findByRole("heading", { name: "Set up your workspace" }),
    ).toBeInTheDocument();
    expect(history.location.pathname).toBe("/setup");
  });
});

describe("an ended session", () => {
  afterEach(() => {
    vi.useRealTimers();
    focusManager.setFocused(undefined);
  });

  it("returns to sign-in when another request finds the session gone", async () => {
    let sessionValid = true;
    const { history } = renderApp("/people", {
      "GET /api/v1/me": () => (sessionValid ? { status: 200, body: me } : replies.unauthorized),
      "GET /api/v1/me/sessions": () => {
        sessionValid = false;
        return replies.unauthorized;
      },
      "GET /api/v1/me/passkeys": { status: 200, body: { items: [] } },
    });
    expect(await screen.findByRole("heading", { name: "People" })).toBeInTheDocument();
    history.push("/settings/security");
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Your session has ended.");
    expect(history.location.search).toContain("redirect=%2Fsettings%2Fsecurity");
  });

  // The person's page is open; the session expires or is revoked elsewhere;
  // they come back to the tab and the stale current person is refetched.
  it("returns to sign-in when a focus refetch of the current person finds no session", async () => {
    let sessionValid = true;
    const { history, user, called } = renderApp("/inbox", {
      "GET /api/v1/me": () => (sessionValid ? { status: 200, body: me } : replies.unauthorized),
      "POST /api/v1/auth/sign-in": () => {
        sessionValid = true;
        return { status: 200, body: signInResult };
      },
    });
    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument();

    sessionValid = false;
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 10 * 60_000); // the current person is stale
    focusManager.setFocused(false);
    focusManager.setFocused(true);

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(called("GET", "/api/v1/me").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByRole("status")).toHaveTextContent("Your session has ended.");
    expect(screen.queryByText("Something went wrong")).not.toBeInTheDocument();
    expect(history.location.pathname).toBe("/sign-in");
    expect(history.location.search).toContain("redirect=%2Finbox");

    // Signing in again returns to the page (the clock stays where it is).
    await user.type(screen.getByLabelText("Work email"), "alex@acme.com");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument();
  });
});
