import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { me, renderApp, replies } from "@/test/harness";

const team = {
  id: "0190a1b2-0000-7000-8000-000000000010",
  name: "Platform",
  slug: "platform",
  description: "Robots and cloud",
  memberCount: 2,
  createdAt: "2026-11-01T10:00:00Z",
  updatedAt: "2026-11-01T10:00:00Z",
};
const path = `/api/v1/teams/${team.id}`;
const admin = { ...me, permissions: ["people.manage"] };
const person = { ...me.person, status: "active", roles: ["member"] };
const routes = {
  "GET /api/v1/me": { status: 200, body: admin },
  "GET /api/v1/teams": { status: 200, body: { items: [team] } },
  [`GET ${path}`]: { status: 200, body: team },
  [`GET ${path}/members`]: { status: 200, body: { items: [] } },
  "GET /api/v1/people": { status: 200, body: { items: [person] } },
};

describe("teams", () => {
  it("lists teams with counts, searches and creates a team", async () => {
    const { user, called, history } = renderApp("/teams", {
      ...routes,
      "POST /api/v1/teams": { status: 201, body: team },
    });
    expect(await screen.findByRole("link", { name: /Platform/ })).toBeInTheDocument();
    expect(screen.getByText("2 members")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Search teams"), "design");
    expect(screen.queryByRole("link", { name: /Platform/ })).not.toBeInTheDocument();
    expect(screen.getByText("No teams found")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "New team" }));
    await user.type(screen.getByLabelText("Team name"), "Platform");
    await user.type(screen.getByLabelText("Slug"), "platform");
    await user.type(screen.getByLabelText("Description"), "Robots and cloud");
    await user.click(screen.getByRole("button", { name: "Create team" }));
    await waitFor(() => expect(history.location.pathname).toBe(`/teams/${team.id}`));
    expect(called("POST", "/api/v1/teams")[0]?.body).toEqual({
      name: "Platform",
      slug: "platform",
      description: "Robots and cloud",
    });
    expect(called("POST", "/api/v1/teams")[0]?.headers.get("X-CSRF-Token")).toBe("csrf-123");
  });

  it("keeps the create form and explains a duplicate slug", async () => {
    const { user } = renderApp("/teams", { ...routes, "POST /api/v1/teams": replies.conflict });
    await user.click(await screen.findByRole("button", { name: "New team" }));
    await user.type(screen.getByLabelText("Team name"), "Platform");
    await user.type(screen.getByLabelText("Slug"), "platform");
    await user.click(screen.getByRole("button", { name: "Create team" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Another team already uses this slug.",
    );
    expect(screen.getByLabelText("Team name")).toHaveValue("Platform");
  });

  it("adds a person, sets a lead and removes the member", async () => {
    let members: { person: typeof person; role: string; joinedAt: string }[] = [];
    const { user, called } = renderApp(`/teams/${team.id}`, {
      ...routes,
      [`GET ${path}/members`]: () => ({ status: 200, body: { items: members } }),
      [`PUT ${path}/members/${person.id}`]: (call) => {
        members = [
          { person, role: (call.body as { role: string }).role, joinedAt: team.createdAt },
        ];
        return { status: 200, body: {} };
      },
      [`DELETE ${path}/members/${person.id}`]: () => {
        members = [];
        return { status: 204 };
      },
    });
    const select = await screen.findByLabelText("Person to add");
    await user.selectOptions(select, person.id);
    await user.click(screen.getByRole("button", { name: "Add member" }));
    await user.click(await screen.findByRole("button", { name: "Make Alex Morgan a lead" }));
    expect(await screen.findByText("Lead")).toBeInTheDocument();
    expect(called("PUT", `${path}/members/${person.id}`).map((c) => c.body)).toEqual([
      { role: "member" },
      { role: "lead" },
    ]);
    await user.click(screen.getByRole("button", { name: "Remove Alex Morgan from team" }));
    expect(await screen.findByText("No members yet")).toBeInTheDocument();
  });

  it("lets a member view teams and leads without edit controls", async () => {
    const { called } = renderApp(`/teams/${team.id}`, {
      ...routes,
      "GET /api/v1/me": { status: 200, body: me },
      [`GET ${path}/members`]: {
        status: 200,
        body: { items: [{ person, role: "lead", joinedAt: team.createdAt }] },
      },
    });
    const heading = await screen.findByRole("heading", { level: 1, name: "Platform" });
    const main = heading.closest("main");
    if (!main) throw new Error("no main");
    expect(await within(main).findByText("Alex Morgan")).toBeInTheDocument();
    expect(within(main).getByText("Lead")).toBeInTheDocument();
    expect(within(main).queryByRole("button")).not.toBeInTheDocument();
    expect(called("GET", "/api/v1/people")).toHaveLength(0);
  });

  it("edits and archives a team after confirmation, keeping its members visible", async () => {
    let current = { ...team, archivedAt: undefined as string | undefined };
    const { user, called } = renderApp(`/teams/${team.id}`, {
      ...routes,
      [`GET ${path}`]: () => ({ status: 200, body: current }),
      [`PATCH ${path}`]: (call) => {
        current = { ...current, ...(call.body as { name: string }) };
        return { status: 200, body: current };
      },
      [`POST ${path}/archive`]: () => {
        current = { ...current, archivedAt: team.createdAt };
        return { status: 200, body: current };
      },
    });
    await user.click(await screen.findByRole("button", { name: "Edit team" }));
    await user.clear(screen.getByLabelText("Team name"));
    await user.type(screen.getByLabelText("Team name"), "Fleet Platform");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(
      await screen.findByRole("heading", { level: 1, name: "Fleet Platform" }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Archive team" }));
    expect(called("POST", `${path}/archive`)).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "Confirm archive" }));
    expect(
      await screen.findByText("This team is archived. Its members and history are kept."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit team" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Person to add")).not.toBeInTheDocument();
  });

  it("explains missing teams", async () => {
    renderApp(`/teams/${team.id}`, {
      ...routes,
      [`GET ${path}`]: { status: 404, body: { title: "Not Found", status: 404 } },
    });
    expect(await screen.findByText("Team not found")).toBeInTheDocument();
  });
});
