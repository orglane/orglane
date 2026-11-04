import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp } from "@/test/harness";

const admin = { ...me, permissions: ["apps.manage", "people.manage"] };
const instanceId = "0190a1b2-0000-7000-8000-0000000000a1";
const feature = (f: string, support: string, milestone?: string) => ({
  feature: f,
  support,
  ...(milestone ? { milestone } : {}),
});
const provider = {
  id: "testprovider",
  name: "Test provider",
  capabilities: ["code"],
  management: "connected",
  features: [
    feature("sso", "unsupported"),
    feature("accounts.create", "supported"),
    feature("deprovision.suspend", "planned", "M5"),
  ],
  credentials: [{ name: "webhook_secret", source: "generated", rotatable: true, agent: false }],
  settings: [],
  employeeSteps: [],
  defaultKey: "test",
  defaultPolicy: "all_people",
  provisioning: true,
  webhooks: true,
  lifecycle: false,
};
const instance = {
  id: instanceId,
  key: "test",
  provider: "testprovider",
  providerAvailable: true,
  management: "connected",
  displayName: "Code",
  state: "active",
  health: "healthy",
  baseUrl: "https://code.acme.test",
  accessPolicy: "all_people",
  manageExistingMembers: false,
  settings: {},
  dataRetained: false,
  createdAt: "2026-11-01T10:00:00Z",
  updatedAt: "2026-11-01T10:00:00Z",
};
const person = (id: string, name: string) => ({
  id,
  username: name.toLowerCase(),
  displayName: name,
  status: "active",
});

describe("Apps", () => {
  it("lists the caller's apps with their state and home URL", async () => {
    renderApp("/apps", {
      "GET /api/v1/me": { status: 200, body: me },
      "GET /api/v1/me/apps": {
        status: 200,
        body: {
          items: [
            {
              instanceId,
              key: "test",
              displayName: "Code",
              provider: "testprovider",
              capabilities: ["code"],
              homeUrl: "https://code.acme.test",
              status: "done",
            },
          ],
        },
      },
    });
    expect(await screen.findByRole("heading", { name: "Code" })).toBeInTheDocument();
    expect(screen.getByText("Up to date")).toBeInTheDocument();
    const open = screen.getByRole("link", { name: "Open" });
    expect(open).toHaveAttribute("href", "https://code.acme.test");
    expect(open).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.queryByRole("link", { name: "Manage apps" })).not.toBeInTheDocument();
  });

  it("shows the empty state and the settings link for app managers", async () => {
    renderApp("/apps", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/me/apps": { status: 200, body: { items: [] } },
    });
    expect(await screen.findByText("No apps connected yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Manage apps" })).toHaveAttribute(
      "href",
      "/settings/apps",
    );
  });
});

describe("Settings → Apps", () => {
  it("does not load anything without apps.manage", async () => {
    const app = renderApp("/settings/apps", { "GET /api/v1/me": { status: 200, body: me } });
    expect(await screen.findByRole("alert")).toHaveTextContent("permission to manage apps");
    expect(app.called("GET", "/api/v1/apps/instances")).toHaveLength(0);
  });

  it("lists instances with state and the honest capability matrix", async () => {
    renderApp("/settings/apps", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/instances": { status: 200, body: { items: [instance] } },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [provider] } },
      "GET /api/v1/apps/bindings": {
        status: 200,
        body: {
          items: [
            {
              capability: "code",
              instanceId,
              instanceKey: "test",
              instanceDisplayName: "Code",
              provider: "testprovider",
              instanceState: "active",
              isDefault: true,
            },
          ],
        },
      },
    });
    const link = await screen.findByRole("link", { name: "Code" });
    expect(link).toHaveAttribute("href", `/settings/apps/${instanceId}`);
    const instances = screen.getByRole("table", { name: "Connected apps" });
    expect(within(instances).getByText("Active")).toBeInTheDocument();
    expect(within(instances).getByText("Default for code")).toBeInTheDocument();
    const matrix = await screen.findByRole("table", { name: "Capability matrix" });
    expect(within(matrix).getByRole("rowheader", { name: "Single sign-on" })).toBeInTheDocument();
    expect(within(matrix).getByText("Not supported")).toBeInTheDocument();
    expect(within(matrix).getByText("Planned (M5)")).toBeInTheDocument();
  });

  it("confirms a held removal batch with the hash it showed", async () => {
    const app = renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [provider] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: {
        status: 200,
        body: {
          instance,
          bindings: [{ capability: "code", isDefault: true }],
          operations: [],
          credentials: [
            {
              name: "webhook_secret",
              version: 1,
              state: "active",
              createdAt: "2026-11-01T10:00:00Z",
            },
          ],
          rotationStuck: false,
          unmanagedAccountCount: 1,
          provisioning: { queued: 0, done: 2, pendingEmployeeAction: 0, failed: 0 },
        },
      },
      [`GET /api/v1/apps/instances/${instanceId}/team-mappings`]: {
        status: 200,
        body: { items: [] },
      },
      "GET /api/v1/teams": { status: 200, body: { items: [] } },
      [`GET /api/v1/apps/instances/${instanceId}/provisioning`]: {
        status: 200,
        body: {
          items: [
            {
              person: person("0190a1b2-0000-7000-8000-0000000000b1", "Priya"),
              status: "done",
              access: true,
              grantedByOrglane: true,
              actions: [
                {
                  kind: "create_account",
                  key: "create_account",
                  outcome: "changed",
                  removal: false,
                },
              ],
              attempts: 0,
              updatedAt: "2026-11-01T10:00:00Z",
            },
          ],
          counts: { queued: 0, done: 2, pendingEmployeeAction: 0, failed: 0 },
          unmanagedAccounts: [
            { externalId: "9", login: "outsider", seenAt: "2026-11-01T10:00:00Z" },
          ],
          heldRemovals: {
            id: "0190a1b2-0000-7000-8000-0000000000c1",
            itemCount: 1,
            itemsHash: "a".repeat(64),
            createdAt: "2026-11-01T10:00:00Z",
            items: [
              {
                id: "0190a1b2-0000-7000-8000-0000000000c2",
                person: person("0190a1b2-0000-7000-8000-0000000000b2", "Sam"),
                kind: "remove_from_org",
                key: "remove_from_org",
              },
            ],
          },
        },
      },
      [`POST /api/v1/apps/instances/${instanceId}/provisioning/removal-batches/0190a1b2-0000-7000-8000-0000000000c1/confirm`]:
        { status: 202 },
    });
    expect(await screen.findByText("Removals waiting for confirmation")).toBeInTheDocument();
    expect(screen.getByText("outsider")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Priya" })).toBeInTheDocument();
    expect(screen.queryByText(/whsec|secret value/)).not.toBeInTheDocument();
    await app.user.click(screen.getByRole("button", { name: "Confirm removals" }));
    await waitFor(() =>
      expect(
        app.called(
          "POST",
          `/api/v1/apps/instances/${instanceId}/provisioning/removal-batches/0190a1b2-0000-7000-8000-0000000000c1/confirm`,
        ),
      ).toHaveLength(1),
    );
    expect(
      app.called(
        "POST",
        `/api/v1/apps/instances/${instanceId}/provisioning/removal-batches/0190a1b2-0000-7000-8000-0000000000c1/confirm`,
      )[0]?.body,
    ).toEqual({ itemsHash: "a".repeat(64) });
    expect(await screen.findByText(/checked again before it is applied/)).toBeInTheDocument();
    // Rotating a credential is a labeled action.
    expect(screen.getByRole("button", { name: "Rotate webhook_secret" })).toBeEnabled();
  });
});
