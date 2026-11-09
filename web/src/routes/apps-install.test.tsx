import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { me, renderApp, replies } from "@/test/harness";

const admin = { ...me, permissions: ["apps.manage", "people.manage"] };
const instanceId = "0190a1b2-0000-7000-8000-0000000000b1";
const forgejo = {
  id: "forgejo",
  name: "Forgejo",
  capabilities: ["code"],
  management: "managed",
  package: "forgejo",
  features: [],
  credentials: [],
  settings: [
    {
      name: "ssh_port",
      type: "int",
      min: 1024,
      max: 65535,
      default: 2222,
      mutable: true,
      required: false,
    },
    {
      name: "organization",
      type: "string",
      pattern: "[A-Za-z0-9]+",
      mutable: false,
      required: true,
    },
  ],
  employeeSteps: [],
  defaultKey: "git",
  defaultPolicy: "all_people",
  provisioning: false,
  webhooks: false,
  lifecycle: true,
  install: {
    key: "git",
    host: "git.acme.com",
    settings: { organization: "acme" },
    packageVersion: "15.0.11-1",
    memoryMin: 256 * 1024 ** 2,
    memoryRecommended: 512 * 1024 ** 2,
    diskMin: 2 * 1024 ** 3,
  },
};
const connected = {
  enabled: true,
  connected: true,
  version: "dev",
  memTotal: 8 * 1024 ** 3,
  memAvailable: 4 * 1024 ** 3,
  diskFree: 50 * 1024 ** 3,
  memoryReserved: 1024 ** 3,
};
const instance = {
  id: instanceId,
  key: "git",
  provider: "forgejo",
  providerAvailable: true,
  management: "managed",
  displayName: "Forgejo",
  state: "active",
  health: "healthy",
  baseUrl: "https://git.acme.com",
  accessPolicy: "all_people",
  manageExistingMembers: false,
  packageId: "forgejo",
  settings: {},
  dataRetained: false,
  createdAt: "2026-11-01T10:00:00Z",
  updatedAt: "2026-11-01T10:00:00Z",
};
const detail = (inst: Record<string, unknown>) => ({
  status: 200,
  body: {
    instance: inst,
    bindings: [],
    operations: [],
    credentials: [],
    rotationStuck: false,
    unmanagedAccountCount: 0,
    provisioning: { queued: 0, done: 0, pendingEmployeeAction: 0, failed: 0 },
  },
});

describe("Install an app", () => {
  it("shows the enable-managed command and no install while the agent is missing", async () => {
    renderApp("/settings/apps/install/forgejo", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      "GET /api/v1/apps/agent": {
        status: 200,
        body: {
          ...connected,
          enabled: false,
          connected: false,
          code: "agent_unavailable",
          memTotal: 0,
        },
      },
    });
    expect(
      await screen.findByRole("heading", { name: "Install Forgejo", level: 1 }),
    ).toBeInTheDocument();
    expect(screen.getByText(/sh -s -- enable-managed/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Install" })).toBeDisabled();
  });

  it("installs with the defaults, checks the input first and explains a refusal", async () => {
    const user = userEvent.setup();
    let body: unknown;
    let refuse = true;
    const app = renderApp("/settings/apps/install/forgejo", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      "GET /api/v1/apps/agent": { status: 200, body: connected },
      "POST /api/v1/apps/instances": (call) => {
        body = call.body;
        if (refuse) {
          refuse = false;
          return {
            status: 409,
            body: { type: "about:blank", title: "Conflict", status: 409, code: "host_taken" },
          };
        }
        return { status: 202, body: { ...instance, state: "installing" } };
      },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail({ ...instance, state: "installing" }),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
    });
    const form = await screen.findByRole("form", { name: "Install Forgejo" });
    // Defaults and resources.
    expect(within(form).getByLabelText("Address")).toHaveValue("git.acme.com");
    expect(within(form).getByLabelText("Key")).toHaveValue("git");
    expect(within(form).getByLabelText("Organization")).toHaveValue("acme");
    expect(within(form).getByLabelText("SSH port")).toHaveValue("2222");
    expect(
      screen.getByText(/Needs at least 256 MB of memory \(512 MB recommended\)/),
    ).toBeInTheDocument();

    // Invalid input is caught before anything is sent, focus on the first.
    const key = within(form).getByLabelText("Key");
    await user.clear(key);
    await user.type(key, "Git!");
    await user.clear(within(form).getByLabelText("SSH port"));
    await user.type(within(form).getByLabelText("SSH port"), "22");
    await user.click(within(form).getByRole("button", { name: "Install" }));
    expect(key).toHaveFocus();
    expect(key).toHaveAccessibleDescription(/lowercase letters/);
    expect(within(form).getByLabelText("SSH port")).toHaveAccessibleDescription(
      "Enter a whole number from 1024 to 65535.",
    );
    expect(app.called("POST", "/api/v1/apps/instances")).toHaveLength(0);

    await user.clear(key);
    await user.type(key, "code");
    await user.clear(within(form).getByLabelText("SSH port"));
    await user.type(within(form).getByLabelText("SSH port"), "2223");
    await user.click(within(form).getByRole("button", { name: "Install" }));
    expect(
      await screen.findByText("Another app uses this address. Choose another one."),
    ).toBeInTheDocument();
    expect(body).toEqual({
      provider: "forgejo",
      key: "code",
      host: "git.acme.com",
      displayName: "Forgejo",
      settings: { ssh_port: 2223, organization: "acme" },
    });

    await user.click(within(form).getByRole("button", { name: "Install" }));
    // The instance page shows it installing.
    expect(await screen.findByRole("heading", { name: "Forgejo", level: 1 })).toBeInTheDocument();
  });
});

describe("Install refusals", () => {
  it("explains a workspace address and moves focus to it", async () => {
    const user = userEvent.setup();
    renderApp("/settings/apps/install/forgejo", {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      "GET /api/v1/apps/agent": { status: 200, body: connected },
      "POST /api/v1/apps/instances": {
        status: 400,
        body: {
          type: "about:blank",
          title: "Bad Request",
          status: 400,
          code: "invalid_input",
          detail: "host: must differ from the workspace host",
        },
      },
    });
    const form = await screen.findByRole("form", { name: "Install Forgejo" });
    await user.click(within(form).getByRole("button", { name: "Install" }));
    const host = within(form).getByLabelText("Address");
    await waitFor(() => expect(host).toHaveFocus());
    expect(host).toHaveAccessibleDescription(/is the workspace's own address/);
    expect(host).toHaveAttribute("aria-invalid", "true");
    expect(screen.queryByText("Something went wrong. Try again.")).not.toBeInTheDocument();
  });
});

describe("Remove, delete permanently, reinstall", () => {
  it("asks before removing, keeps focus, and needs the key to delete permanently", async () => {
    const user = userEvent.setup();
    const deletes: string[] = [];
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail(instance),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
      [`DELETE /api/v1/apps/instances/${instanceId}`]: (call) => {
        deletes.push(call.query.toString());
        return { status: 202, body: { ...instance, state: "removing" } };
      },
    });
    const remove = await screen.findByRole("button", { name: "Remove" });
    await user.click(remove);
    const panel = screen.getByRole("group", { name: "Remove" });
    expect(panel).toHaveAccessibleDescription(/data, database and settings are kept/);
    expect(within(panel).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.click(within(panel).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove" })).toHaveFocus());

    await user.click(screen.getByRole("button", { name: "Delete permanently" }));
    const purge = screen.getByRole("group", { name: "Delete permanently" });
    expect(purge).toHaveAccessibleDescription(/cannot be undone/);
    const confirm = within(purge).getByRole("button", { name: "Delete all data" });
    expect(confirm).toBeDisabled();
    const typed = within(purge).getByLabelText(/type the app's key git/);
    expect(typed).toHaveFocus();
    await user.type(typed, "gi");
    expect(confirm).toBeDisabled();
    await user.type(typed, "t");
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await waitFor(() => expect(deletes).toEqual(["purge=true&confirm=git"]));
  });

  it("reinstalls a removed app whose data is kept", async () => {
    const user = userEvent.setup();
    let reinstalled = false;
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail({
        ...instance,
        state: "removed",
        dataRetained: true,
      }),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
      [`POST /api/v1/apps/instances/${instanceId}/reinstall`]: () => {
        reinstalled = true;
        return { status: 202, body: { ...instance, state: "installing" } };
      },
    });
    expect(
      await screen.findByText(/Reinstalling brings it back with its data/),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete permanently" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Reinstall" }));
    await waitFor(() => expect(reinstalled).toBe(true));
  });

  it("cancels a confirmation with Escape from any control and returns focus", async () => {
    const user = userEvent.setup();
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail(instance),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Delete permanently" }));
    const purge = screen.getByRole("group", { name: "Delete permanently" });
    await user.type(within(purge).getByLabelText(/type the app's key git/), "git");
    const confirm = within(purge).getByRole("button", { name: "Delete all data" });
    confirm.focus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("group", { name: "Delete permanently" })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Delete permanently" })).toHaveFocus(),
    );
    // From the Remove confirmation's Cancel button too.
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove" })).toHaveFocus());
  });

  it("keeps a pending purge open, so a failure is not lost with a canceled panel", async () => {
    const user = userEvent.setup();
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    let query = "";
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail(instance),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
      [`DELETE /api/v1/apps/instances/${instanceId}`]: async (call) => {
        query = call.query.toString();
        await pending;
        return replies.conflict;
      },
    });
    await user.click(await screen.findByRole("button", { name: "Delete permanently" }));
    const purge = screen.getByRole("group", { name: "Delete permanently" });
    const typed = within(purge).getByLabelText(/type the app's key git/);
    await user.type(typed, "git");
    await user.click(within(purge).getByRole("button", { name: "Delete all data" }));
    await waitFor(() => expect(query).toBe("purge=true&confirm=git"));

    // The request cannot be aborted: Escape and Cancel leave the panel and
    // its mutation state alone until it settles.
    typed.focus();
    await user.keyboard("{Escape}");
    const cancel = within(purge).getByRole("button", { name: "Cancel" });
    expect(screen.getByRole("group", { name: "Delete permanently" })).toBeInTheDocument();
    expect(cancel).toBeDisabled();
    await user.click(cancel);
    expect(screen.getByRole("group", { name: "Delete permanently" })).toBeInTheDocument();

    // Once it fails, the panel stays and shows the refusal; Cancel works
    // again.
    release();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await waitFor(() => expect(cancel).toBeEnabled());
    await user.click(cancel);
    expect(screen.queryByRole("group", { name: "Delete permanently" })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Delete permanently" })).toHaveFocus(),
    );
  });

  it("after a failed purge offers only to delete permanently again", async () => {
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail({
        ...instance,
        state: "failed",
        health: "unhealthy",
        healthCode: "purge_failed",
      }),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
    });
    expect(await screen.findByText(/can only be deleted permanently again/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete permanently" })).toBeInTheDocument();
  });

  it("lets a long name wrap on the instance page", async () => {
    const long = "x".repeat(100);
    renderApp(`/settings/apps/${instanceId}`, {
      "GET /api/v1/me": { status: 200, body: admin },
      "GET /api/v1/apps/providers": { status: 200, body: { items: [forgejo] } },
      [`GET /api/v1/apps/instances/${instanceId}`]: detail({ ...instance, displayName: long }),
      [`GET /api/v1/apps/instances/${instanceId}/operations`]: {
        status: 200,
        body: { items: [], hasMore: false },
      },
    });
    const heading = await screen.findByRole("heading", { name: long, level: 1 });
    expect(heading.className).toContain("[overflow-wrap:anywhere]");
  });
});
