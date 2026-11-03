import { screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { me, renderApp } from "@/test/harness";

it("distinguishes a failed manager lookup from no manager and retries the stored relationship", async () => {
  const managerId = "0190a1b2-0000-7000-8000-000000000002";
  const person = {
    ...me.person,
    status: "active",
    roles: [],
    teams: [],
    updatedAt: "2026-11-03T00:00:00Z",
    managerId,
  };
  let attempts = 0;
  const app = renderApp(`/people/${person.id}`, {
    "GET /api/v1/me": { status: 200, body: me },
    [`GET /api/v1/people/${person.id}`]: { status: 200, body: person },
    [`GET /api/v1/people/${person.id}/profile`]: {
      status: 200,
      body: { personId: person.id, fields: [] },
    },
    [`GET /api/v1/people/${managerId}`]: () =>
      ++attempts === 1
        ? { status: 503, body: { type: "about:blank", title: "Unavailable", status: 503 } }
        : {
            status: 200,
            body: { ...person, id: managerId, displayName: "Maya Okafor", managerId: undefined },
          },
  });
  expect(await screen.findByText("Manager profile unavailable.")).toBeVisible();
  expect(screen.queryByText("No manager set")).not.toBeInTheDocument();
  await app.user.click(screen.getByRole("button", { name: "Try again" }));
  const link = await screen.findByRole("link", { name: "Maya Okafor" });
  expect(link).toHaveAttribute("href", `/people/${managerId}`);
  expect(screen.queryByText("Manager profile unavailable.")).not.toBeInTheDocument();
  expect(app.called("PATCH", `/api/v1/people/${person.id}`)).toHaveLength(0);
});
