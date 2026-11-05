import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
    expect(await screen.findByText("No apps assigned to you yet")).toBeInTheDocument();
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
    let confirmed = false;
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
      // After the confirmation the batch is no longer held.
      [`GET /api/v1/apps/instances/${instanceId}/provisioning`]: () => ({
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
          heldRemovals: confirmed
            ? undefined
            : {
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
      }),
      [`POST /api/v1/apps/instances/${instanceId}/provisioning/removal-batches/0190a1b2-0000-7000-8000-0000000000c1/confirm`]:
        () => {
          confirmed = true;
          return { status: 202 };
        },
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
    await waitFor(() =>
      expect(screen.queryByText("Removals waiting for confirmation")).not.toBeInTheDocument(),
    );
    expect(screen.getByText(/checked again before it is applied/)).toBeInTheDocument();
    // Rotating a credential is a labeled action.
    expect(screen.getByRole("button", { name: "Rotate webhook_secret" })).toBeEnabled();
  });
});

const detailWith = (credentials: unknown[], extra: Record<string, unknown> = {}) => ({
  instance: { ...instance, ...extra },
  bindings: [],
  operations: [],
  credentials,
  rotationStuck: true,
  unmanagedAccountCount: 0,
  provisioning: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 0 },
});

describe("Instance page guidance", () => {
  it("offers to cancel a stalled rotation and keep the current value", async () => {
    const abandonPath = `/api/v1/apps/instances/${instanceId}/credentials/webhook_secret/abandon`;
    const app = renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [provider] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: {
        status: 200,
        body: detailWith(
          [
            {
              name: "webhook_secret",
              version: 1,
              state: "active",
              createdAt: "2026-11-01T10:00:00Z",
            },
            {
              name: "webhook_secret",
              version: 2,
              state: "uncertain",
              createdAt: "2026-11-01T10:00:00Z",
            },
          ],
          { state: "degraded", health: "unhealthy", healthCode: "agent_unavailable" },
        ),
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
              status: "failed",
              access: true,
              grantedByOrglane: true,
              actions: [],
              errorCode: "seat_limit",
              attempts: 1,
              updatedAt: "2026-11-01T10:00:00Z",
            },
          ],
          counts: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 1 },
          unmanagedAccounts: [],
        },
      },
      [`POST ${abandonPath}`]: { status: 202 },
    });
    // Failure states explain themselves; codes stay secondary.
    expect(await screen.findByText(/Waiting for the agent that runs this app/)).toBeInTheDocument();
    expect(await screen.findByText(/The app has no free seats/)).toBeInTheDocument();
    // A stalled rotation says what happens and offers the way out.
    expect(screen.getByText(/Waiting for the provider to confirm version 2/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rotate webhook_secret" })).not.toBeInTheDocument();
    await app.user.click(
      screen.getByRole("button", {
        name: "Cancel the rotation of webhook_secret and keep the current value",
      }),
    );
    await waitFor(() => expect(app.called("POST", abandonPath)).toHaveLength(1));
  });

  it("keeps refreshing after a mapping until the background work shows up", async () => {
    let syncs = 0;
    const provisioningPath = `/api/v1/apps/instances/${instanceId}/provisioning`;
    const app = renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [provider] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: { status: 200, body: detailWith([]) },
      [`GET /api/v1/apps/instances/${instanceId}/team-mappings`]: {
        status: 200,
        body: { items: [] },
      },
      "GET /api/v1/teams": {
        status: 200,
        body: {
          items: [
            {
              id: "0190a1b2-0000-7000-8000-0000000000d1",
              slug: "platform",
              name: "Platform",
              memberCount: 1,
              createdAt: "2026-11-01T10:00:00Z",
              updatedAt: "2026-11-01T10:00:00Z",
            },
          ],
        },
      },
      [`PUT /api/v1/apps/instances/${instanceId}/team-mappings/0190a1b2-0000-7000-8000-0000000000d1`]:
        {
          status: 200,
          body: {},
        },
      [`GET ${provisioningPath}`]: () => {
        syncs++;
        return {
          status: 200,
          body: {
            items:
              syncs > 2
                ? [
                    {
                      person: person("0190a1b2-0000-7000-8000-0000000000b1", "Priya"),
                      status: "done",
                      access: true,
                      grantedByOrglane: true,
                      actions: [],
                      attempts: 0,
                      updatedAt: "2026-11-01T10:00:00Z",
                    },
                  ]
                : [],
            counts: { queued: 0, done: syncs > 2 ? 1 : 0, pendingEmployeeAction: 0, failed: 0 },
            unmanagedAccounts: [],
          },
        };
      },
    });
    expect(await screen.findByText("Nobody has access yet.")).toBeInTheDocument();
    await app.user.selectOptions(screen.getByLabelText("Team to map"), "Platform");
    await app.user.click(screen.getByRole("button", { name: "Map team" }));
    // Not a definitive empty state while the jobs run.
    expect(await screen.findByText(/Applying changes in the background/)).toBeInTheDocument();
    expect(screen.queryByText("Nobody has access yet.")).not.toBeInTheDocument();
    expect(
      await screen.findByRole("link", { name: "Priya" }, { timeout: 8000 }),
    ).toBeInTheDocument();
  }, 15_000);
});

describe("Apps guidance", () => {
  it("explains a failed access and what to do", async () => {
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
              status: "failed",
            },
          ],
        },
      },
    });
    expect(await screen.findByText(/ask an administrator/)).toBeInTheDocument();
  });

  it("shows a managed install waiting for the agent and its operation history", async () => {
    const managed = {
      ...instance,
      key: "managed-test",
      provider: "agenttest",
      management: "managed",
      displayName: "Managed",
      state: "installing",
      health: "unknown",
      packageId: "agenttest",
      waitingForAgent: "agent_unavailable",
    };
    const op = (o: Record<string, unknown>) => ({
      kind: "apply",
      createdAt: "2026-11-01T10:00:00Z",
      toVersion: "1.0.0-1",
      ...o,
    });
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": {
        status: 200,
        body: { items: [{ ...provider, id: "agenttest", management: "managed" }] },
      },
      [`GET /api/v1/apps/instances/${instanceId}`]: {
        status: 200,
        body: {
          instance: managed,
          bindings: [],
          operations: [],
          credentials: [],
          rotationStuck: false,
          unmanagedAccountCount: 0,
          provisioning: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 0 },
        },
      },
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: ({ query }) => ({
        status: 200,
        body:
          query.get("offset") === "3"
            ? {
                items: [op({ id: "o0", reason: "reinstall", state: "succeeded" })],
                hasMore: false,
              }
            : {
                items: [
                  op({
                    id: "o3",
                    generation: 3,
                    reason: "resync",
                    state: "queued",
                    errorCode: "agent_unavailable",
                  }),
                  op({
                    id: "o2",
                    generation: 2,
                    reason: "configure",
                    state: "failed",
                    errorCode: "superseded",
                    finishedAt: "2026-11-01T10:01:00Z",
                  }),
                  op({
                    id: "o1",
                    generation: 1,
                    reason: "install",
                    state: "succeeded",
                    finishedAt: "2026-11-01T10:02:00Z",
                  }),
                ],
                hasMore: true,
              },
      }),
    });
    const alert = await screen.findByText(/^To start the agent/);
    expect(alert.parentElement).toHaveTextContent("enable-managed");
    const table = await screen.findByRole("table", { name: "Operations" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("Restore after agent data loss");
    expect(rows[1]).toHaveTextContent("Waiting for the agent");
    expect(rows[2]).toHaveTextContent("Replaced by a newer change before it ran.");
    expect(rows[3]).toHaveTextContent("Install");
    expect(rows[3]).toHaveTextContent("Succeeded");
    // Older history pages in after the ones shown.
    await userEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(await within(table).findByText("Reinstall")).toBeInTheDocument();
    expect(within(table).getAllByRole("row")).toHaveLength(5);
    expect(screen.queryByRole("button", { name: "Show more" })).not.toBeInTheDocument();
  });

  it("keeps the loaded history and keyboard focus when more operations load, and names phases", async () => {
    const managed = {
      ...instance,
      key: "managed-test",
      provider: "agenttest",
      management: "managed",
      displayName: "Managed",
      packageId: "agenttest",
    };
    const op = (o: Record<string, unknown>) => ({
      kind: "apply",
      createdAt: "2026-11-01T10:00:00Z",
      toVersion: "1.0.0-1",
      ...o,
    });
    let olderCalls = 0;
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": {
        status: 200,
        body: { items: [{ ...provider, id: "agenttest", management: "managed" }] },
      },
      [`GET /api/v1/apps/instances/${instanceId}`]: {
        status: 200,
        body: {
          instance: managed,
          bindings: [],
          operations: [],
          credentials: [],
          rotationStuck: false,
          unmanagedAccountCount: 0,
          provisioning: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 0 },
        },
      },
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: ({ query }) => {
        if (query.get("offset") !== "2") {
          return {
            status: 200,
            body: {
              items: [
                op({
                  id: "o2",
                  generation: 2,
                  reason: "configure",
                  state: "running",
                  phase: "policy_check",
                }),
                op({
                  id: "o1",
                  generation: 1,
                  reason: "resync",
                  state: "running",
                  phase: "frobnicating",
                }),
              ],
              hasMore: true,
            },
          };
        }
        olderCalls++;
        // The first request for older operations fails (403: not retried).
        return olderCalls === 1
          ? {
              status: 403,
              body: { type: "about:blank", title: "Forbidden", status: 403 },
            }
          : {
              status: 200,
              body: {
                items: [op({ id: "o0", generation: 0, reason: "install", state: "succeeded" })],
                hasMore: false,
              },
            };
      },
    });
    const table = await screen.findByRole("table", { name: "Operations" });
    // Known phases are named; an unknown one stays readable.
    expect(within(table).getByText("Checking the configuration")).toBeInTheDocument();
    expect(within(table).getByText("frobnicating").parentElement).toHaveTextContent(
      "In progress: frobnicating",
    );

    const user = userEvent.setup();
    const more = screen.getByRole("button", { name: "Show more" });
    more.focus();
    await user.keyboard("{Enter}");
    // A failed page keeps the loaded history, the control and the focus.
    expect(await screen.findByText(/Older operations could not be loaded/)).toBeInTheDocument();
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    const again = screen.getByRole("button", { name: "Try again to show more" });
    expect(again).toHaveFocus();

    await user.keyboard("{Enter}");
    const install = await within(table).findByText("Install");
    // Focus moves to the first new row and the append is announced.
    expect(install.closest("th")).toHaveFocus();
    expect(screen.getByText("1 more operation loaded.")).toBeInTheDocument();
    expect(screen.queryByText(/Older operations could not be loaded/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /show more/i })).not.toBeInTheDocument();
  });
});
