import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { me, renderApp } from "@/test/harness";

describe("Profile field settings", () => {
  it("does not fetch definitions without people.manage", async () => {
    const app = renderApp("/settings/profile-fields", {
      "GET /api/v1/me": { status: 200, body: me },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("permission to manage people");
    expect(app.called("GET", "/api/v1/profile-fields")).toHaveLength(0);
    expect(app.called("GET", "/api/v1/profile-field-library")).toHaveLength(0);
  });
  it("creates a select field in three steps with collapsed advanced settings", async () => {
    const app = renderApp("/settings/profile-fields", {
      "GET /api/v1/me": { status: 200, body: { ...me, permissions: ["people.manage"] } },
      "GET /api/v1/profile-fields": { status: 200, body: { items: [] } },
      "GET /api/v1/profile-field-library": { status: 200, body: { items: [] } },
      "POST /api/v1/profile-fields": { status: 201, body: {} },
    });
    await app.user.click(await screen.findByRole("button", { name: "Create custom field" }));
    await app.user.type(screen.getByLabelText("Field name"), "Office");
    await app.user.click(screen.getByRole("button", { name: "Next" }));
    await app.user.selectOptions(screen.getByLabelText("Field type"), "select");
    await app.user.type(screen.getByLabelText("Choices (one per line)"), "Berlin\nLisbon");
    await app.user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByLabelText("Required at onboarding")).not.toBeVisible();
    await app.user.selectOptions(screen.getByLabelText("Visibility"), "team");
    await app.user.click(screen.getByRole("button", { name: /^Create field$/ }));
    await waitFor(() => expect(app.called("POST", "/api/v1/profile-fields")).toHaveLength(1));
    expect(app.called("POST", "/api/v1/profile-fields")[0]?.body).toEqual({
      label: "Office",
      type: "select",
      visibility: "team",
      section: "custom",
      editableBy: "self",
      requiredAtOnboarding: false,
      userCanHide: false,
      sensitive: false,
      options: [{ label: "Berlin" }, { label: "Lisbon" }],
    });
  });
});
