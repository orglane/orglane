import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Schema } from "@/lib/api";
import { me, renderApp } from "@/test/harness";

const field: Schema<"ProfileField"> = {
  id: "0190a1b2-0000-7000-8000-000000000002",
  key: "telegram",
  label: "Telegram",
  library: true,
  type: "handle",
  handleKind: "telegram",
  options: [],
  section: "contacts",
  visibility: "everyone",
  requiredAtOnboarding: false,
  editableBy: "self",
  userCanHide: false,
  sensitive: false,
  position: 1,
  archived: false,
};
const entry: Schema<"ProfileFieldValue"> = {
  field,
  value: "alex_tg",
  hidden: true,
  yearHidden: false,
  editable: true,
  hideable: false,
};

describe("Profile field hiding", () => {
  it("lets the profile subject unhide after an admin disables further hiding", async () => {
    const path = `/api/v1/people/${me.person.id}/profile`;
    const app = renderApp(`/people/${me.person.id}`, {
      "GET /api/v1/me": { status: 200, body: me },
      [`GET /api/v1/people/${me.person.id}`]: {
        status: 200,
        body: {
          ...me.person,
          status: "active",
          roles: [],
          teams: [],
          updatedAt: "2026-11-03T00:00:00Z",
        },
      },
      [`GET ${path}`]: { status: 200, body: { personId: me.person.id, fields: [entry] } },
      [`PATCH ${path}`]: {
        status: 200,
        body: { personId: me.person.id, fields: [{ ...entry, hidden: false }] },
      },
    });
    const checkbox = await screen.findByRole("checkbox", { name: "Hide from others" });
    expect(checkbox).toBeChecked();
    await app.user.click(checkbox);
    await waitFor(() =>
      expect(screen.queryByText("Hidden from others", { exact: true })).not.toBeInTheDocument(),
    );
    expect(screen.queryByRole("checkbox", { name: "Hide from others" })).not.toBeInTheDocument();
    expect(app.called("PATCH", path)[0]?.body).toEqual({
      values: [{ key: "telegram", hidden: false }],
    });
  });
  it("does not give another person's admin a hide or unhide control", async () => {
    const personId = "0190a1b2-0000-7000-8000-000000000003";
    renderApp(`/people/${personId}`, {
      "GET /api/v1/me": {
        status: 200,
        body: { ...me, permissions: ["people.manage", "people.sensitive.read"] },
      },
      [`GET /api/v1/people/${personId}`]: {
        status: 200,
        body: {
          ...me.person,
          id: personId,
          status: "active",
          roles: [],
          teams: [],
          updatedAt: "2026-11-03T00:00:00Z",
        },
      },
      [`GET /api/v1/people/${personId}/profile`]: {
        status: 200,
        body: { personId, fields: [{ ...entry, editable: true }] },
      },
    });
    expect(await screen.findByText("Hidden from others", { exact: true })).toBeVisible();
    expect(screen.queryByRole("checkbox", { name: "Hide from others" })).not.toBeInTheDocument();
  });
});
