import { queryOptions } from "@tanstack/react-query";
import { defineMessage, defineMessages } from "react-intl";

import { type Api, call, type Schema } from "./api";

export const peopleKey = ["people"] as const;
export const personKey = (personId: string) => [...peopleKey, "person", personId] as const;

export function personQuery(api: Api, personId: string) {
  return queryOptions({
    queryKey: personKey(personId),
    queryFn: () =>
      call(api.client.GET("/api/v1/people/{personId}", { params: { path: { personId } } })),
    retry: false,
  });
}

// The existing team selector needs every option, including people beyond the
// first directory page. It follows the same public cursor contract as clients.
export async function allPeople(api: Api): Promise<{ items: Schema<"Person">[] }> {
  const items: Schema<"Person">[] = [];
  let cursor: string | undefined;
  do {
    const page = await call(
      api.client.GET("/api/v1/people", {
        params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } },
      }),
    );
    items.push(...page.items);
    cursor = page.nextCursor;
  } while (cursor);
  return { items };
}

export const peopleMessages = defineMessages({
  import: { id: "people.import", defaultMessage: "Import people" },
  title: { id: "nav.people", defaultMessage: "People" },
  invite: { id: "people.invite", defaultMessage: "Invite people" },
  description: { id: "people.description", defaultMessage: "Find anyone in your company." },
  search: { id: "people.search", defaultMessage: "Search people" },
  searchHint: {
    id: "people.searchHint",
    defaultMessage: "Search by name, username, email or title.",
  },
  team: { id: "people.team", defaultMessage: "Team" },
  allTeams: { id: "people.allTeams", defaultMessage: "All teams" },
  role: { id: "people.role", defaultMessage: "Role" },
  allRoles: { id: "people.allRoles", defaultMessage: "All roles" },
  owner: { id: "people.role.owner", defaultMessage: "Owner" },
  admin: { id: "people.role.admin", defaultMessage: "Admin" },
  member: { id: "people.role.member", defaultMessage: "Member" },
  active: { id: "people.status.active", defaultMessage: "Active" },
  invited: { id: "people.status.invited", defaultMessage: "Invited" },
  suspended: { id: "people.status.suspended", defaultMessage: "Suspended" },
  offboarded: { id: "people.status.offboarded", defaultMessage: "Offboarded" },
  empty: { id: "people.empty", defaultMessage: "No people found" },
  emptyHint: { id: "people.emptyHint", defaultMessage: "Try another search or clear the filters." },
  clear: { id: "people.clear", defaultMessage: "Clear filters" },
  next: { id: "people.next", defaultMessage: "Next page" },
  previous: { id: "people.previous", defaultMessage: "Previous page" },
  page: defineMessage<{ page: number }>({ id: "people.page", defaultMessage: "Page {page}" }),
  count: defineMessage<{ count: number }>({
    id: "people.count",
    defaultMessage: "{count, plural, one {# person on this page} other {# people on this page}}",
  }),
  loading: { id: "common.loading", defaultMessage: "Loading" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
  back: { id: "people.back", defaultMessage: "All people" },
  profile: { id: "people.profile", defaultMessage: "Profile" },
  edit: { id: "people.edit", defaultMessage: "Edit profile" },
  cancel: { id: "common.cancel", defaultMessage: "Cancel" },
  save: { id: "people.save", defaultMessage: "Save profile" },
  saved: { id: "people.saved", defaultMessage: "Profile saved." },
  name: { id: "people.name", defaultMessage: "Full name" },
  jobTitle: { id: "people.jobTitle", defaultMessage: "Job title" },
  username: { id: "people.username", defaultMessage: "Username" },
  email: { id: "people.email", defaultMessage: "Work email" },
  contact: { id: "people.contact", defaultMessage: "Contact" },
  teams: { id: "people.teams", defaultMessage: "Teams" },
  noTeams: { id: "people.noTeams", defaultMessage: "No teams yet" },
  manager: { id: "people.manager", defaultMessage: "Reports to" },
  noManager: { id: "people.noManager", defaultMessage: "No manager set" },
  managerUnavailable: {
    id: "people.managerUnavailable",
    defaultMessage: "Manager profile unavailable.",
  },
  managerSearch: { id: "people.managerSearch", defaultMessage: "Search for a manager" },
  managerHint: {
    id: "people.managerHint",
    defaultMessage: "Search by name, email or title, then choose a person.",
  },
  notFound: { id: "people.notFound", defaultMessage: "Person not found" },
  invalid: {
    id: "people.invalid",
    defaultMessage: "Check the profile fields. A manager must not create a reporting cycle.",
  },
  conflict: { id: "people.conflict", defaultMessage: "That username or email is already in use." },
  avatar: { id: "people.avatar", defaultMessage: "Upload avatar" },
  avatarHint: {
    id: "people.avatarHint",
    defaultMessage: "PNG, JPEG or WebP, up to 5 MB. SVG files are not supported.",
  },
  avatarInvalid: {
    id: "people.avatarInvalid",
    defaultMessage: "Choose a valid PNG, JPEG or WebP under 5 MB and 16 megapixels.",
  },
  avatarSaved: { id: "people.avatarSaved", defaultMessage: "Avatar updated." },
});
