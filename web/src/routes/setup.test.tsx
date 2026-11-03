import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp, replies, signInResult } from "@/test/harness";

import { suggestUsername } from "./setup";

const needsSetup = { "GET /api/v1/auth/setup": { status: 200, body: { required: true } } };

async function fill(
  user: ReturnType<typeof renderApp>["user"],
  overrides: Partial<Record<string, string>> = {},
) {
  const v = {
    token: "ost_token",
    name: "Alex Morgan",
    email: "Alex.Morgan@acme.com",
    password: "correct horse battery",
    ...overrides,
  };
  await user.type(await screen.findByLabelText("Setup token"), v.token);
  await user.type(screen.getByLabelText("Full name"), v.name);
  await user.type(screen.getByLabelText("Work email"), v.email);
  await user.type(screen.getByLabelText("Password"), v.password);
}

describe("first-run setup", () => {
  it("suggests a username from the email", () => {
    expect(suggestUsername("Alex.Morgan+x@acme.com")).toBe("alex.morganx");
    expect(suggestUsername("_x@acme.com")).toBe("x");
  });

  it("validates before sending", async () => {
    const { user, called } = renderApp("/setup", needsSetup);
    await fill(user, { password: "short" });
    await user.click(screen.getByRole("button", { name: "Create owner account" }));
    expect(screen.getByText("Use at least 12 characters.")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Password")).toHaveFocus();
    expect(called("POST", "/api/v1/auth/setup")).toHaveLength(0);
  });

  it("explains an invalid setup token on the field", async () => {
    const { user } = renderApp("/setup", {
      ...needsSetup,
      "POST /api/v1/auth/setup": replies.forbidden,
    });
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Create owner account" }));
    expect(await screen.findByText(/This setup token isn't valid/)).toBeInTheDocument();
    expect(screen.getByLabelText("Setup token")).toHaveAttribute("aria-invalid", "true");
  });

  it("creates the owner and offers a passkey when the browser supports it", async () => {
    Object.defineProperty(window, "PublicKeyCredential", {
      value: function PublicKeyCredential() {},
      configurable: true,
    });
    Object.defineProperty(navigator, "credentials", {
      value: { create: () => Promise.reject(), get: () => Promise.reject() },
      configurable: true,
    });
    let created = false;
    const { user, called, history } = renderApp("/setup", {
      ...needsSetup,
      "POST /api/v1/auth/setup": () => {
        created = true;
        return { status: 201, body: { ...signInResult, passkeyOffer: true } };
      },
      "GET /api/v1/me": () =>
        created ? { status: 200, body: { ...me, passkeyOffer: true } } : replies.unauthorized,
    });
    await fill(user);
    expect(screen.getByLabelText("Username")).toHaveValue("alex.morgan");
    await user.click(screen.getByRole("button", { name: "Create owner account" }));
    expect(
      await screen.findByRole("heading", { name: "Sign in faster with a passkey" }),
    ).toBeInTheDocument();
    expect(history.location.pathname).toBe("/welcome/passkey");
    expect(called("POST", "/api/v1/auth/setup")[0]?.body).toEqual({
      setupToken: "ost_token",
      displayName: "Alex Morgan",
      workEmail: "Alex.Morgan@acme.com",
      username: "alex.morgan",
      password: "correct horse battery",
    });
    // @ts-expect-error cleanup of the stub
    delete window.PublicKeyCredential;
  });
});
