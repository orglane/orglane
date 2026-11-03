import { screen } from "@testing-library/react";
import { expect, it } from "vitest";

import { me, renderApp } from "@/test/harness";

it("shows configured SMTP as read-only, never populates the password", async () => {
  renderApp("/settings/email", {
    "GET /api/v1/me": { status: 200, body: { ...me, permissions: ["instance.manage"] } },
    "GET /api/v1/settings/email": {
      status: 200,
      body: {
        source: "config",
        host: "smtp.example.test",
        port: 587,
        tls: "starttls",
        username: "mailer",
        passwordSet: true,
        from: "no-reply@acme.com",
      },
    },
  });
  expect(await screen.findByLabelText("SMTP host")).toBeDisabled();
  expect(screen.getByLabelText("SMTP password")).toHaveValue("");
  expect(screen.queryByRole("button", { name: "Save email settings" })).not.toBeInTheDocument();
});

it("lets an admin keep the stored password and displays test delivery errors", async () => {
  const { user, called } = renderApp("/settings/email", {
    "GET /api/v1/me": { status: 200, body: { ...me, permissions: ["instance.manage"] } },
    "GET /api/v1/settings/email": {
      status: 200,
      body: {
        source: "settings",
        host: "smtp.example.test",
        port: 587,
        tls: "starttls",
        username: "mailer",
        passwordSet: true,
        from: "no-reply@acme.com",
      },
    },
    "PUT /api/v1/settings/email": { status: 200, body: { source: "settings" } },
    "POST /api/v1/settings/email/test": {
      status: 502,
      body: { type: "about:blank", status: 502, title: "Bad Gateway" },
    },
  });
  await user.click(await screen.findByRole("button", { name: "Save email settings" }));
  expect(await screen.findByRole("status")).toHaveTextContent("saved");
  expect(called("PUT", "/api/v1/settings/email")[0]?.body).not.toHaveProperty("password");
  await user.type(screen.getByLabelText("Test recipient"), "owner@example.test");
  await user.click(screen.getByRole("button", { name: "Send test email" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("SMTP server could not deliver");
});
