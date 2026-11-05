import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { me, renderApp } from "@/test/harness";

const manager = { ...me, permissions: ["apps.manage"] };
const instanceId = "0190a1b2-0000-7000-8000-0000000000a1";

describe("GitHub connection", () => {
  it("requires app management permission before exposing the connect form", async () => {
    const app = renderApp("/settings/apps/github/connect", {
      "GET /api/v1/me": { status: 200, body: me },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("permission to manage apps");
    expect(screen.queryByRole("button", { name: "Prepare GitHub App" })).not.toBeInTheDocument();
    expect(app.called("POST", "/api/v1/apps/github/connect")).toHaveLength(0);
  });
  it("prepares a named organization connection through the public API and presents its manifest handoff", async () => {
    const app = renderApp("/settings/apps/github/connect", {
      "GET /api/v1/me": { status: 200, body: manager },
      "POST /api/v1/apps/github/connect": {
        status: 200,
        body: {
          instanceId,
          done: false,
          name: "create_app",
          method: "POST",
          url: "https://github.com/organizations/orglane-test/settings/apps/new?state=ols_example",
          form: { manifest: '{"name":"Orglane test"}' },
        },
      },
    });
    await app.user.type(await screen.findByLabelText("GitHub organization"), "orglane-test");
    await app.user.click(screen.getByRole("button", { name: "Prepare GitHub App" }));
    expect(
      await screen.findByRole("heading", { name: "Create your GitHub App" }),
    ).toBeInTheDocument();
    expect(app.called("POST", "/api/v1/apps/github/connect")[0]?.body).toEqual({
      organization: "orglane-test",
      key: "github",
    });
    const submit = screen.getByRole("button", { name: "Continue to GitHub" });
    expect(submit.closest("form")).toHaveAttribute("method", "POST");
    expect(submit.closest("form")).toHaveAttribute(
      "action",
      "https://github.com/organizations/orglane-test/settings/apps/new?state=ols_example",
    );
    expect(screen.getByRole("link", { name: "View connection" })).toHaveAttribute(
      "href",
      `/settings/apps/${instanceId}`,
    );
  });
  it("keeps the form available after a rejected preparation", async () => {
    const app = renderApp("/settings/apps/github/connect", {
      "GET /api/v1/me": { status: 200, body: manager },
      "POST /api/v1/apps/github/connect": {
        status: 409,
        body: { type: "about:blank", status: 409, title: "Conflict", code: "key_reserved" },
      },
    });
    await app.user.type(await screen.findByLabelText("GitHub organization"), "orglane-test");
    await app.user.click(screen.getByRole("button", { name: "Prepare GitHub App" }));
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByLabelText("GitHub organization")).toHaveValue("orglane-test");
    expect(screen.getByRole("button", { name: "Prepare GitHub App" })).toBeEnabled();
    expect(screen.getByRole("alert")).toHaveTextContent("Choose a different app key");
    expect(screen.getByLabelText("App key in Orglane")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("App key in Orglane")).toHaveAccessibleDescription(
      /Choose a different app key/,
    );
  });
});

const fixture = {
  id: instanceId,
  key: "github",
  provider: "github",
  providerAvailable: true,
  management: "connected",
  displayName: "GitHub fixture",
  state: "active",
  health: "healthy",
  accessPolicy: "all_people",
  manageExistingMembers: false,
  settings: { organization: "orglane-test" },
  dataRetained: false,
  createdAt: "2026-11-01T10:00:00Z",
  updatedAt: "2026-11-01T10:00:00Z",
};
const githubProvider = {
  id: "github",
  name: "GitHub",
  capabilities: ["code"],
  management: "connected",
  features: [],
  credentials: [{ name: "app_private_key", source: "upload", rotatable: true, agent: false }],
  settings: [],
  employeeSteps: [],
  defaultKey: "github",
  defaultPolicy: "all_people",
  provisioning: false,
  webhooks: false,
  lifecycle: false,
};
const detail = {
  instance: fixture,
  bindings: [],
  operations: [],
  credentials: [
    { name: "app_private_key", version: 1, state: "active", createdAt: fixture.createdAt },
  ],
  rotationStuck: false,
  unmanagedAccountCount: 0,
  provisioning: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 0 },
};

it("explains a locally invalid manual key at the retained PEM field", async () => {
  const app = renderApp(`/settings/apps/${instanceId}`, {
    "GET /api/v1/me": { status: 200, body: manager },
    "GET /api/v1/apps/providers": { status: 200, body: { items: [githubProvider] } },
    [`GET /api/v1/apps/instances/${instanceId}`]: {
      status: 200,
      body: { ...detail, instance: { ...fixture, state: "connecting" } },
    },
    "POST /api/v1/apps/github/connect": {
      status: 400,
      body: {
        type: "about:blank",
        title: "Bad Request",
        status: 400,
        code: "invalid_private_key",
        detail: "Unsafe provider diagnostics",
      },
    },
  });
  await app.user.click(await screen.findByRole("button", { name: "Use manual setup" }));
  for (const [label, value] of [
    ["App ID", "123"],
    ["Client ID", "client"],
    ["Client secret", "retained-secret"],
    ["Webhook secret (at least 32 characters)", "a".repeat(32)],
    ["Private key (PEM)", "invalid-pem"],
  ] as const)
    await app.user.type(screen.getByLabelText(label), value);
  await app.user.click(screen.getByRole("button", { name: "Resume GitHub connection" }));
  const error = await screen.findByRole("alert");
  expect(error).toHaveTextContent("Generate a private key in this GitHub App");
  expect(error).not.toHaveTextContent("Unsafe provider diagnostics");
  expect(screen.getByLabelText("Private key (PEM)")).toHaveValue("invalid-pem");
  expect(screen.getByLabelText("Client secret")).toHaveValue("retained-secret");
  expect(screen.getByLabelText("Private key (PEM)")).toHaveAttribute("aria-invalid", "true");
  expect(screen.getByLabelText("Private key (PEM)")).toHaveAccessibleDescription(
    /Generate a private key/,
  );
});

it.each([
  ["accepted", "Replacement key accepted and queued"],
  ["verifying", "Verifying the replacement key"],
  ["failed", "Replacement key verification failed"],
  ["verified", "Replacement key verified"],
])("shows the persisted %s rotation outcome on a fresh page", async (state, text) => {
  renderApp(`/settings/apps/${instanceId}`, {
    "GET /api/v1/me": { status: 200, body: manager },
    "GET /api/v1/apps/providers": { status: 200, body: { items: [githubProvider] } },
    [`GET /api/v1/apps/instances/${instanceId}`]: {
      status: 200,
      body: { ...detail, credentialRotations: [{ name: "app_private_key", version: 2, state }] },
    },
  });
  expect(await screen.findByText(new RegExp(text))).toBeVisible();
  if (state === "failed") {
    expect(screen.getByRole("alert")).toHaveTextContent("The working key is still active");
    expect(
      screen.queryByText(/Replacement key accepted and queued for verification/),
    ).not.toBeInTheDocument();
    expect(screen.getByLabelText("New private key (.pem)")).toBeEnabled();
  }
});

it("never presents an admin's unverified activation as verified or advises revoking the old key", async () => {
  renderApp(`/settings/apps/${instanceId}`, {
    "GET /api/v1/me": { status: 200, body: manager },
    "GET /api/v1/apps/providers": { status: 200, body: { items: [githubProvider] } },
    [`GET /api/v1/apps/instances/${instanceId}`]: {
      status: 200,
      body: {
        ...detail,
        credentialRotations: [{ name: "app_private_key", version: 2, state: "activated_by_admin" }],
      },
    },
  });
  const status = await screen.findByText(/activated by an admin without verification/);
  expect(status).toBeVisible();
  expect(screen.queryByText(/verified and active/)).not.toBeInTheDocument();
  expect(screen.queryByText(/You can now revoke/)).not.toBeInTheDocument();
  expect(status).toHaveTextContent("Keep the old key on GitHub");
});

it("explains an upload refused while the key is still being changed", async () => {
  const uploadPath = `/api/v1/apps/instances/${instanceId}/credentials/app_private_key`;
  const app = renderApp(`/settings/apps/${instanceId}`, {
    "GET /api/v1/me": { status: 200, body: manager },
    "GET /api/v1/apps/providers": { status: 200, body: { items: [githubProvider] } },
    [`GET /api/v1/apps/instances/${instanceId}`]: { status: 200, body: detail },
    [`PUT ${uploadPath}`]: {
      status: 409,
      body: { type: "about:blank", title: "Conflict", status: 409, code: "rotation_in_progress" },
    },
  });
  const file = new File(["test-only-pem"], "key.pem", { type: "application/x-pem-file" });
  // jsdom's File has no text(); browsers do.
  Object.defineProperty(file, "text", { value: async () => "test-only-pem" });
  const input = await screen.findByLabelText("New private key (.pem)");
  await app.user.upload(input, file);
  // jsdom's constraint validation does not see files set by user-event.
  const form = input.closest("form");
  if (!form) throw new Error("no form");
  fireEvent.submit(form);
  await waitFor(() => expect(app.called("PUT", uploadPath)).toHaveLength(1));
  expect(await screen.findByText(/Orglane is still changing this key/)).toBeVisible();
  expect(screen.queryByText(/Something went wrong/)).not.toBeInTheDocument();
});

it("never flashes a callback failure during a valid callback's initial commit", async () => {
  window.history.replaceState(
    null,
    "",
    "/settings/apps/github/callback?state=ols_example&code=example",
  );
  const alerts: string[] = [];
  const observer = new MutationObserver((records) => {
    for (const record of records)
      for (const node of record.addedNodes)
        if (node instanceof Element) {
          const found = [
            ...(node.matches('[role="alert"]') ? [node] : []),
            ...node.querySelectorAll('[role="alert"]'),
          ];
          alerts.push(...found.map((n) => n.textContent ?? ""));
        }
  });
  observer.observe(document.body, { subtree: true, childList: true });
  try {
    const app = renderApp("/settings/apps/github/callback", {
      "GET /api/v1/me": { status: 200, body: manager },
      "POST /api/v1/apps/github/connect/complete": {
        status: 200,
        body: { instanceId, done: true },
      },
    });
    expect(
      await screen.findByRole("heading", { name: "GitHub connected", level: 1 }),
    ).toBeVisible();
    expect(app.called("POST", "/api/v1/apps/github/connect/complete")).toHaveLength(1);
    expect(alerts.join(" ")).not.toContain("This connection could not be completed");
  } finally {
    observer.disconnect();
    window.history.replaceState(null, "", "/");
  }
});
