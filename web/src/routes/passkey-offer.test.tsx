import { screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { me, renderApp } from "@/test/harness";

const create = vi.fn();

beforeEach(() => {
  Object.defineProperty(window, "PublicKeyCredential", {
    value: function PublicKeyCredential() {},
    configurable: true,
  });
  Object.defineProperty(navigator, "credentials", {
    value: { create, get: vi.fn() },
    configurable: true,
  });
});

afterEach(() => {
  // @ts-expect-error cleanup of the stub
  delete window.PublicKeyCredential;
  create.mockReset();
});

const signedIn = { "GET /api/v1/me": { status: 200, body: { ...me, passkeyOffer: true } } };

describe("passkey offer", () => {
  it("skips, remembers it, and continues", async () => {
    const { user, called, history } = renderApp("/welcome/passkey?redirect=%2Fpeople", {
      ...signedIn,
      "POST /api/v1/me/passkey-offer/dismiss": { status: 204 },
    });
    await user.click(await screen.findByRole("button", { name: "Skip for now" }));
    expect(await screen.findByRole("heading", { name: "People" })).toBeInTheDocument();
    expect(history.location.pathname).toBe("/people");
    expect(called("POST", "/api/v1/me/passkey-offer/dismiss")).toHaveLength(1);
    expect(called("POST", "/api/v1/me/passkey-offer/dismiss")[0]?.headers.get("X-CSRF-Token")).toBe(
      "csrf-123",
    );
  });

  it("stays quiet when the person cancels the browser prompt", async () => {
    create.mockRejectedValue(
      new DOMException("The operation either timed out or was not allowed.", "NotAllowedError"),
    );
    const { user } = renderApp("/welcome/passkey", {
      ...signedIn,
      "POST /api/v1/me/passkeys/options": {
        status: 200,
        body: {
          ceremonyId: "c1",
          publicKey: {
            challenge: "AAEC",
            rp: { name: "Orglane" },
            user: { id: "AQID", name: "a", displayName: "A" },
            pubKeyCredParams: [],
          },
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Add a passkey" }));
    expect(create).toHaveBeenCalledOnce();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add a passkey" })).toBeEnabled();
  });
});
