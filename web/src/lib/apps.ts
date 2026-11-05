import { queryOptions } from "@tanstack/react-query";
import { defineMessages, type IntlShape, type MessageDescriptor } from "react-intl";

import { type Api, call, type Schema } from "./api";

export const appsKey = ["apps"] as const;

export type AppInstance = Schema<"AppInstance">;
export type AppProvider = Schema<"AppProvider">;

export function appProvidersQuery(api: Api) {
  return queryOptions({
    queryKey: [...appsKey, "providers"],
    queryFn: () => call(api.client.GET("/api/v1/apps/providers")),
  });
}

export function appInstancesQuery(api: Api) {
  return queryOptions({
    queryKey: [...appsKey, "instances"],
    queryFn: () => call(api.client.GET("/api/v1/apps/instances")),
  });
}

export function appBindingsQuery(api: Api) {
  return queryOptions({
    queryKey: [...appsKey, "bindings"],
    queryFn: () => call(api.client.GET("/api/v1/apps/bindings")),
  });
}

export function appInstanceQuery(api: Api, instanceId: string) {
  return queryOptions({
    queryKey: [...appsKey, "instances", instanceId],
    queryFn: () =>
      call(
        api.client.GET("/api/v1/apps/instances/{instanceId}", { params: { path: { instanceId } } }),
      ),
  });
}

export function appTeamMappingsQuery(api: Api, instanceId: string) {
  return queryOptions({
    queryKey: [...appsKey, "instances", instanceId, "team-mappings"],
    queryFn: () =>
      call(
        api.client.GET("/api/v1/apps/instances/{instanceId}/team-mappings", {
          params: { path: { instanceId } },
        }),
      ),
  });
}

export function appProvisioningQuery(api: Api, instanceId: string) {
  return queryOptions({
    queryKey: [...appsKey, "instances", instanceId, "provisioning"],
    queryFn: () =>
      call(
        api.client.GET("/api/v1/apps/instances/{instanceId}/provisioning", {
          params: { path: { instanceId }, query: { limit: 200 } },
        }),
      ),
  });
}

export function myAppsQuery(api: Api) {
  return queryOptions({
    queryKey: [...appsKey, "mine"],
    queryFn: () => call(api.client.GET("/api/v1/me/apps")),
  });
}

export function personAppsQuery(api: Api, personId: string) {
  return queryOptions({
    queryKey: [...appsKey, "people", personId],
    queryFn: () =>
      call(api.client.GET("/api/v1/people/{personId}/apps", { params: { path: { personId } } })),
  });
}

export const stateMessages = defineMessages({
  connecting: { id: "apps.state.connecting", defaultMessage: "Connecting" },
  installing: { id: "apps.state.installing", defaultMessage: "Installing" },
  active: { id: "apps.state.active", defaultMessage: "Active" },
  degraded: { id: "apps.state.degraded", defaultMessage: "Degraded" },
  upgrading: { id: "apps.state.upgrading", defaultMessage: "Upgrading" },
  failed: { id: "apps.state.failed", defaultMessage: "Failed" },
  disconnected: { id: "apps.state.disconnected", defaultMessage: "Disconnected" },
  removing: { id: "apps.state.removing", defaultMessage: "Removing" },
  removed: { id: "apps.state.removed", defaultMessage: "Removed" },
}) satisfies Record<Schema<"AppInstanceState">, MessageDescriptor>;

export function stateVariant(state: Schema<"AppInstanceState">): "success" | "warning" | "neutral" {
  if (state === "active") return "success";
  if (state === "degraded" || state === "failed" || state === "disconnected") return "warning";
  return "neutral";
}

export const healthMessages = defineMessages({
  unknown: { id: "apps.health.unknown", defaultMessage: "Health unknown" },
  healthy: { id: "apps.health.healthy", defaultMessage: "Healthy" },
  unhealthy: { id: "apps.health.unhealthy", defaultMessage: "Unhealthy" },
}) satisfies Record<Schema<"AppHealth">, MessageDescriptor>;

export const statusMessages = defineMessages({
  queued: { id: "apps.status.queued", defaultMessage: "Updating" },
  done: { id: "apps.status.done", defaultMessage: "Up to date" },
  pending_employee_action: {
    id: "apps.status.pendingEmployeeAction",
    defaultMessage: "Needs action",
  },
  failed: { id: "apps.status.failed", defaultMessage: "Failed" },
}) satisfies Record<Schema<"ProvisioningStatus">, MessageDescriptor>;

export function statusVariant(
  status: Schema<"ProvisioningStatus">,
): "success" | "warning" | "neutral" {
  if (status === "done") return "success";
  if (status === "failed" || status === "pending_employee_action") return "warning";
  return "neutral";
}

export const supportMessages = defineMessages({
  supported: { id: "apps.support.supported", defaultMessage: "Supported" },
  partial: { id: "apps.support.partial", defaultMessage: "Partial" },
  planned: { id: "apps.support.planned", defaultMessage: "Planned" },
  unsupported: { id: "apps.support.unsupported", defaultMessage: "Not supported" },
}) satisfies Record<Schema<"AppFeatureSupport">["support"], MessageDescriptor>;

/** Capability matrix rows (docs/specs/providers.md "Capability matrix"). */
export const featureMessages: Record<string, MessageDescriptor> = defineMessages({
  sso: { id: "apps.feature.sso", defaultMessage: "Single sign-on" },
  deep_links: { id: "apps.feature.deepLinks", defaultMessage: "Deep links" },
  "accounts.create": { id: "apps.feature.accountsCreate", defaultMessage: "Create accounts" },
  "accounts.invite": { id: "apps.feature.accountsInvite", defaultMessage: "Invite accounts" },
  "accounts.link": { id: "apps.feature.accountsLink", defaultMessage: "Link accounts" },
  "teams.sync": { id: "apps.feature.teamsSync", defaultMessage: "Sync teams" },
  "deprovision.team_removal": {
    id: "apps.feature.deprovisionTeamRemoval",
    defaultMessage: "Remove from teams",
  },
  "deprovision.org_removal": {
    id: "apps.feature.deprovisionOrgRemoval",
    defaultMessage: "Remove from the organization",
  },
  "deprovision.suspend": {
    id: "apps.feature.deprovisionSuspend",
    defaultMessage: "Suspend accounts",
  },
  "deprovision.revoke_tokens": {
    id: "apps.feature.deprovisionRevokeTokens",
    defaultMessage: "Revoke tokens and keys",
  },
  "events.webhooks": { id: "apps.feature.eventsWebhooks", defaultMessage: "Events from webhooks" },
  "lifecycle.install": { id: "apps.feature.lifecycleInstall", defaultMessage: "Install" },
  "lifecycle.upgrade": { id: "apps.feature.lifecycleUpgrade", defaultMessage: "Upgrade" },
  "lifecycle.snapshot": { id: "apps.feature.lifecycleSnapshot", defaultMessage: "Snapshots" },
  "lifecycle.backup": { id: "apps.feature.lifecycleBackup", defaultMessage: "Backups" },
});

export const outcomeMessages = defineMessages({
  changed: { id: "apps.outcome.changed", defaultMessage: "Changed" },
  unchanged: { id: "apps.outcome.unchanged", defaultMessage: "Already done" },
  failed: { id: "apps.outcome.failed", defaultMessage: "Failed" },
  not_attempted: { id: "apps.outcome.notAttempted", defaultMessage: "Not attempted" },
  held: { id: "apps.outcome.held", defaultMessage: "Waiting for the removal check" },
}) satisfies Record<Schema<"ProvisioningActionResult">["outcome"], MessageDescriptor>;

export const actionMessages: Record<string, MessageDescriptor> = defineMessages({
  create_account: { id: "apps.action.createAccount", defaultMessage: "Create account" },
  update_account: { id: "apps.action.updateAccount", defaultMessage: "Update account" },
  invite: { id: "apps.action.invite", defaultMessage: "Invite" },
  cancel_invitation: { id: "apps.action.cancelInvitation", defaultMessage: "Cancel invitation" },
  add_to_team: { id: "apps.action.addToTeam", defaultMessage: "Add to team" },
  remove_from_team: { id: "apps.action.removeFromTeam", defaultMessage: "Remove from team" },
  remove_from_org: { id: "apps.action.removeFromOrg", defaultMessage: "Remove from organization" },
});

export const managementMessages = defineMessages({
  managed: { id: "apps.management.managed", defaultMessage: "Run by Orglane" },
  connected: { id: "apps.management.connected", defaultMessage: "Connected" },
}) satisfies Record<Schema<"AppManagement">, MessageDescriptor>;

export const policyMessages = defineMessages({
  all_people: { id: "apps.policy.allPeople", defaultMessage: "Everyone in the company" },
  mapped_teams: { id: "apps.policy.mappedTeams", defaultMessage: "Members of mapped teams" },
}) satisfies Record<Schema<"AppAccessPolicy">, MessageDescriptor>;

/** Formats `key` from a message map, falling back to the key itself. */
export function label(
  intl: IntlShape,
  map: Record<string, MessageDescriptor>,
  key: string,
): string {
  const m = map[key];
  return m ? intl.formatMessage(m) : key;
}

/**
 * Explanations of the stable codes of instance states and provisioning
 * results: what happened, whether Orglane retries, and what to do. The
 * code itself stays visible as a secondary detail.
 */
export const codeMessages: Record<string, MessageDescriptor> = defineMessages({
  setup_failed: {
    id: "apps.code.setupFailed",
    defaultMessage: "Setting up this app failed. Check the service, then retry the setup.",
  },
  agent_unavailable: {
    id: "apps.code.agentUnavailable",
    defaultMessage:
      "Waiting for the agent that runs this app. Orglane keeps trying; check that the agent is running.",
  },
  install_failed: {
    id: "apps.code.installFailed",
    defaultMessage: "Installing this app failed. Check the server's resources, then retry.",
  },
  webhook_deleted: {
    id: "apps.code.webhookDeleted",
    defaultMessage: "The app no longer sends events to Orglane. Reconnect it to restore them.",
  },
  provider_unavailable: {
    id: "apps.code.providerUnavailable",
    defaultMessage: "This version of Orglane cannot talk to this app.",
  },
  provider_error: {
    id: "apps.code.providerError",
    defaultMessage: "The app did not answer as expected. Orglane retries automatically.",
  },
  rate_limited: {
    id: "apps.code.rateLimited",
    defaultMessage: "The app asked Orglane to slow down. It retries once the app allows it.",
  },
  seat_limit: {
    id: "apps.code.seatLimit",
    defaultMessage: "The app has no free seats. Add seats in the app, then retry.",
  },
  owner_protected: {
    id: "apps.code.ownerProtected",
    defaultMessage: "Owners of the app's organization are never removed by Orglane.",
  },
  ownership_unproven: {
    id: "apps.code.ownershipUnproven",
    defaultMessage:
      "This person's account already exists in the app, but Orglane cannot prove it created it, so it is not managed and never removed automatically. Review it, then use Retry failed to accept it as unmanaged.",
  },
  invalid_login: {
    id: "apps.code.invalidLogin",
    defaultMessage: "The app refused this person's username. Change the username, then retry.",
  },
});

export const guidanceMessages = defineMessages({
  instanceProblem: {
    id: "apps.guidance.instanceProblem",
    defaultMessage:
      "This app has a problem. Orglane keeps checking it; if it stays like this, check the service.",
  },
  resultFailed: {
    id: "apps.guidance.resultFailed",
    defaultMessage:
      "Orglane couldn't update this person's access. Fix the cause in the app, then use Retry failed.",
  },
  myFailed: {
    id: "apps.guidance.myFailed",
    defaultMessage:
      "We couldn't update your access to this app. If it stays like this, ask an administrator.",
  },
  myQueued: {
    id: "apps.guidance.myQueued",
    defaultMessage: "Your access is being set up. This usually takes a moment.",
  },
  myAction: {
    id: "apps.guidance.myAction",
    defaultMessage: "This app needs something from you before you can use it.",
  },
});

/** The explanation of a code, if it is a known one. */
export function explainCode(intl: IntlShape, code: string | undefined): string | undefined {
  const m = code ? codeMessages[code] : undefined;
  return m ? intl.formatMessage(m) : undefined;
}
