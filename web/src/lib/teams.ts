import { queryOptions } from "@tanstack/react-query";

import { type Api, call } from "./api";

export const teamsKey = ["teams"] as const;

export function teamsQuery(api: Api, archived = false) {
  return queryOptions({
    queryKey: [...teamsKey, { archived }],
    queryFn: () => call(api.client.GET("/api/v1/teams", { params: { query: { archived } } })),
  });
}

export function teamQuery(api: Api, teamId: string) {
  return queryOptions({
    queryKey: [...teamsKey, teamId],
    queryFn: () => call(api.client.GET("/api/v1/teams/{teamId}", { params: { path: { teamId } } })),
  });
}

export function teamMembersQuery(api: Api, teamId: string) {
  return queryOptions({
    queryKey: [...teamsKey, teamId, "members"],
    queryFn: () =>
      call(api.client.GET("/api/v1/teams/{teamId}/members", { params: { path: { teamId } } })),
  });
}
