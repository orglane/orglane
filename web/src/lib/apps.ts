import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query";
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

/** The provider's teams for "existing" mappings, read from the provider. */
export function appProviderTeamsQuery(api: Api, instanceId: string) {
  return queryOptions({
    queryKey: [...appsKey, "instances", instanceId, "provider-teams"],
    queryFn: () =>
      call(
        api.client.GET("/api/v1/apps/instances/{instanceId}/provider-teams", {
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

export type AppOperation = Schema<"AppOperation">;
export type AppAgentStatus = Schema<"AppAgentStatus">;

/** Whether the agent runs, with the host's resources (managed services). */
export function appAgentQuery(api: Api) {
  return queryOptions({
    queryKey: [...appsKey, "agent"],
    queryFn: () => call(api.client.GET("/api/v1/apps/agent")),
  });
}

/** Labels and hints of provider settings, by setting name; others show the name. */
export const settingMessages: Record<
  string,
  { label: MessageDescriptor; hint?: MessageDescriptor }
> = {
  organization: {
    label: { id: "apps.setting.organization", defaultMessage: "Organization" },
    hint: {
      id: "apps.setting.organization.hint",
      defaultMessage: "The company's organization in the app. It cannot be changed later.",
    },
  },
  ssh_port: {
    label: { id: "apps.setting.sshPort", defaultMessage: "SSH port" },
    hint: {
      id: "apps.setting.sshPort.hint",
      defaultMessage:
        "The port on this server for Git over SSH. Open it in the firewall to clone over SSH.",
    },
  },
  require_link: {
    label: { id: "apps.setting.requireLink", defaultMessage: "Require linked accounts" },
  },
};

/** A byte count in MB or GB (binary units, as the agent reports them). */
export function formatBytes(intl: IntlShape, bytes: number): string {
  const gib = 1024 ** 3;
  return bytes >= gib
    ? intl.formatNumber(bytes / gib, { style: "unit", unit: "gigabyte", maximumFractionDigits: 1 })
    : intl.formatNumber(bytes / 1024 ** 2, {
        style: "unit",
        unit: "megabyte",
        maximumFractionDigits: 0,
      });
}

/** Operations per page of a managed instance's history. */
export const OPERATIONS_PAGE = 20;

/** A managed instance's agent operations, newest first, a page at a time. */
export function appOperationsQuery(api: Api, instanceId: string) {
  return infiniteQueryOptions({
    queryKey: [...appsKey, "instances", instanceId, "operations"],
    initialPageParam: 0,
    queryFn: ({ pageParam }) =>
      call(
        api.client.GET("/api/v1/apps/instances/{instanceId}/operations", {
          params: { path: { instanceId }, query: { limit: OPERATIONS_PAGE, offset: pageParam } },
        }),
      ),
    getNextPageParam: (last, pages) =>
      last.hasMore ? pages.reduce((n, p) => n + p.items.length, 0) : undefined,
  });
}

/** Codes of unfinished operations that wait for the agent. */
export const waitingCodes = new Set([
  "agent_unavailable",
  "agent_version_mismatch",
  "agent_untrusted",
]);

export function operationFinished(op: AppOperation): boolean {
  return op.state === "succeeded" || op.state === "failed" || op.state === "rolled_back";
}

export const operationStateMessages = defineMessages({
  queued: { id: "apps.operation.state.queued", defaultMessage: "Queued" },
  running: { id: "apps.operation.state.running", defaultMessage: "Running" },
  awaiting_confirmation: {
    id: "apps.operation.state.awaitingConfirmation",
    defaultMessage: "Checking the new version",
  },
  succeeded: { id: "apps.operation.state.succeeded", defaultMessage: "Succeeded" },
  failed: { id: "apps.operation.state.failed", defaultMessage: "Failed" },
  rolled_back: { id: "apps.operation.state.rolledBack", defaultMessage: "Rolled back" },
}) satisfies Record<AppOperation["state"], MessageDescriptor>;

export function operationStateVariant(op: AppOperation): "success" | "warning" | "neutral" {
  if (op.state === "succeeded") return "success";
  if (op.state === "failed" || op.state === "rolled_back") {
    return op.errorCode === "superseded" ? "neutral" : "warning";
  }
  return op.errorCode && waitingCodes.has(op.errorCode) ? "warning" : "neutral";
}

/** What an operation did: by the reason of its desired state, else its kind. */
export const operationMessages: Record<string, MessageDescriptor> = defineMessages({
  install: { id: "apps.operation.install", defaultMessage: "Install" },
  configure: { id: "apps.operation.configure", defaultMessage: "Configuration change" },
  upgrade: { id: "apps.operation.upgrade", defaultMessage: "Upgrade" },
  retry: { id: "apps.operation.retry", defaultMessage: "Upgrade retry" },
  revert: { id: "apps.operation.revert", defaultMessage: "Return to the running version" },
  remove: { id: "apps.operation.remove", defaultMessage: "Removal" },
  reinstall: { id: "apps.operation.reinstall", defaultMessage: "Reinstall" },
  resync: { id: "apps.operation.resync", defaultMessage: "Restore after agent data loss" },
  apply: { id: "apps.operation.apply", defaultMessage: "Apply" },
  snapshot: { id: "apps.operation.snapshot", defaultMessage: "Snapshot" },
  restore: { id: "apps.operation.restore", defaultMessage: "Restore" },
});

/**
 * Phases the agent reports for unfinished operations (docs/specs/agent.md
 * "Operations" and "Upgrade"). Unknown phases fall back to a generic label
 * with the raw phase as detail.
 */
export const phaseMessages: Record<string, MessageDescriptor> = defineMessages({
  rendering: { id: "apps.phase.rendering", defaultMessage: "Preparing the configuration" },
  policy_check: { id: "apps.phase.policyCheck", defaultMessage: "Checking the configuration" },
  preparing: { id: "apps.phase.preparing", defaultMessage: "Preparing files and data" },
  database: { id: "apps.phase.database", defaultMessage: "Preparing the database" },
  starting: { id: "apps.phase.starting", defaultMessage: "Starting the services" },
  configuring: { id: "apps.phase.configuring", defaultMessage: "Configuring the app" },
  stopping: { id: "apps.phase.stopping", defaultMessage: "Stopping the services" },
  removing: { id: "apps.phase.removing", defaultMessage: "Removing the services" },
  purging: { id: "apps.phase.purging", defaultMessage: "Deleting the data" },
  preflight: { id: "apps.phase.preflight", defaultMessage: "Checking the upgrade" },
  pulling: { id: "apps.phase.pulling", defaultMessage: "Downloading the new version" },
  snapshotting: { id: "apps.phase.snapshotting", defaultMessage: "Taking a snapshot" },
  switching: { id: "apps.phase.switching", defaultMessage: "Switching to the new version" },
  migrating: { id: "apps.phase.migrating", defaultMessage: "Migrating the data" },
  verifying: { id: "apps.phase.verifying", defaultMessage: "Checking the new version" },
  awaiting_confirmation: {
    id: "apps.phase.awaitingConfirmation",
    defaultMessage: "Waiting for Orglane's check",
  },
  committing: { id: "apps.phase.committing", defaultMessage: "Finishing the upgrade" },
  rolling_back: {
    id: "apps.phase.rollingBack",
    defaultMessage: "Returning to the previous version",
  },
  restored: { id: "apps.phase.restored", defaultMessage: "Restarting the previous version" },
});

export const waitingMessages: Record<string, MessageDescriptor> = defineMessages({
  agent_unavailable: {
    id: "apps.waiting.agentUnavailable",
    defaultMessage: "Waiting for the agent",
  },
  agent_version_mismatch: {
    id: "apps.waiting.agentVersionMismatch",
    defaultMessage: "Waiting for the agent: other version",
  },
  agent_untrusted: {
    id: "apps.waiting.agentUntrusted",
    defaultMessage: "Waiting for the agent: not trusted",
  },
});

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
  installation_deleted: {
    id: "apps.code.installationDeleted",
    defaultMessage:
      "The GitHub App was uninstalled from the organization. Orglane no longer receives its events or manages access there; connect GitHub again to resume.",
  },
  installation_suspended: {
    id: "apps.code.installationSuspended",
    defaultMessage:
      "The GitHub App's installation is suspended. Unsuspend it in the organization's settings; this clears when GitHub reports it.",
  },
  agent_unavailable: {
    id: "apps.code.agentUnavailable",
    defaultMessage:
      "Waiting for the agent that runs this app. Orglane keeps trying; check that the agent is running.",
  },
  agent_version_mismatch: {
    id: "apps.code.agentVersionMismatch",
    defaultMessage:
      "The agent runs another version of Orglane, so Orglane sends it no changes. Update Orglane and the agent together (docker compose pull, then up -d).",
  },
  agent_untrusted: {
    id: "apps.code.agentUntrusted",
    defaultMessage:
      "The agent's answers are not signed with the shared secret. Check that Orglane and the agent use the same ORGLANE_AGENT_SECRET.",
  },
  agent_unhealthy: {
    id: "apps.code.agentUnhealthy",
    defaultMessage:
      "A service of this app is not running or not healthy. The agent keeps it running; check its logs if this stays.",
  },
  agent_instance_missing: {
    id: "apps.code.agentInstanceMissing",
    defaultMessage: "The agent does not know this app yet. Orglane sends it again.",
  },
  health_check_failed: {
    id: "apps.code.healthCheckFailed",
    defaultMessage: "The app runs but does not answer as expected. Orglane keeps checking it.",
  },
  install_failed: {
    id: "apps.code.installFailed",
    defaultMessage:
      "Installing this app failed; the operations below say why. Once the cause is fixed, remove the app and reinstall it, or delete it permanently and install it again.",
  },
  remove_failed: {
    id: "apps.code.removeFailed",
    defaultMessage: "Removing this app failed. Check the agent's logs, then remove it again.",
  },
  key_reserved: {
    id: "apps.code.keyReserved",
    defaultMessage:
      "Another app uses this key, or a removed app's data still holds it. Choose another key, or delete that app's data.",
  },
  host_taken: {
    id: "apps.code.hostTaken",
    defaultMessage: "Another app uses this address. Choose another one.",
  },
  localhost_workspace: {
    id: "apps.code.localhostWorkspace",
    defaultMessage:
      "Apps run by Orglane need a workspace address under localhost, such as orglane.localhost. Run the installer's enable-managed command on the server.",
  },
  host_not_local: {
    id: "apps.code.hostNotLocal",
    defaultMessage: "On this local installation, the address must end in .localhost.",
  },
  insufficient_memory: {
    id: "apps.code.insufficientMemory",
    defaultMessage:
      "This server does not have enough memory for this app next to the ones already installed.",
  },
  insufficient_disk: {
    id: "apps.code.insufficientDisk",
    defaultMessage: "This server does not have enough free disk space for this app.",
  },
  dns_unresolved: {
    id: "apps.code.dnsUnresolved",
    defaultMessage:
      "This address does not exist in DNS yet. Add a record that points to this server, then try again.",
  },
  dns_mismatch: {
    id: "apps.code.dnsMismatch",
    defaultMessage:
      "This address points to another server. Point it to the same address as the workspace, then try again.",
  },
  dns_lookup_failed: {
    id: "apps.code.dnsLookupFailed",
    defaultMessage: "The DNS lookup failed. Try again in a moment.",
  },
  not_supported: {
    id: "apps.code.notSupported",
    defaultMessage: "This app cannot do that.",
  },
  superseded: {
    id: "apps.code.superseded",
    defaultMessage: "Replaced by a newer change before it ran.",
  },
  generation_conflict: {
    id: "apps.code.generationConflict",
    defaultMessage: "The address settings changed while this ran. Orglane sends the change again.",
  },
  invalid_input: {
    id: "apps.code.invalidInput",
    defaultMessage: "The agent refused the app's settings.",
  },
  package_unavailable: {
    id: "apps.code.packageUnavailable",
    defaultMessage: "This version of the app is not part of this Orglane build. Update Orglane.",
  },
  credential_missing: {
    id: "apps.code.credentialMissing",
    defaultMessage:
      "A credential this change was recorded with no longer exists. Make the change again.",
  },
  policy_violation: {
    id: "apps.code.policyViolation",
    defaultMessage: "The agent refused the app's configuration because it breaks a safety rule.",
  },
  pull_failed: {
    id: "apps.code.pullFailed",
    defaultMessage:
      "The agent could not download the app's images. Check the server's internet access.",
  },
  compose_failed: {
    id: "apps.code.composeFailed",
    defaultMessage: "The agent could not start the app's services. Check the agent's logs.",
  },
  database_failed: {
    id: "apps.code.databaseFailed",
    defaultMessage: "The agent could not prepare the app's database. Check the agent's logs.",
  },
  hook_failed: {
    id: "apps.code.hookFailed",
    defaultMessage: "Configuring the app's services failed. Check the agent's logs.",
  },
  health_timeout: {
    id: "apps.code.healthTimeout",
    defaultMessage: "The app's services did not become healthy in time. Check the agent's logs.",
  },
  webhook_deleted: {
    id: "apps.code.webhookDeleted",
    defaultMessage:
      "The app no longer sends events to Orglane: its webhook was deleted. Set the webhook URL and secret again in the app (for GitHub, in the App's settings); the next event it delivers clears this.",
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
  username_taken: {
    id: "apps.code.usernameTaken",
    defaultMessage:
      "Another account in the app already has this person's username, and Orglane never takes over an account it did not create. Rename or remove that account in the app, then retry.",
  },
  account_refused: {
    id: "apps.code.accountRefused",
    defaultMessage:
      "The app refused this person's account details, for example an email address another account uses. Fix them, then retry.",
  },
  no_account: {
    id: "apps.code.noAccount",
    defaultMessage: "The account is not there yet. Orglane retries automatically.",
  },
  sso_not_configured: {
    id: "apps.code.ssoNotConfigured",
    defaultMessage:
      "Sign-in with Orglane is not set up in the app yet. Orglane retries; if this stays, reinstall the app.",
  },
  service_admin_invalid: {
    id: "apps.code.serviceAdminInvalid",
    defaultMessage:
      "Orglane cannot sign in to the app as its administrator. Orglane retries; if this stays, check the app's service account.",
  },
  forgejo_unavailable: {
    id: "apps.code.forgejoUnavailable",
    defaultMessage: "Forgejo did not answer. Orglane retries automatically.",
  },
  team_not_synced: {
    id: "apps.code.teamNotSynced",
    defaultMessage:
      "A team this person is mapped to does not exist in the app yet. Orglane creates it and retries automatically.",
  },
  team_not_managed: {
    id: "apps.code.teamNotManaged",
    defaultMessage:
      "A team this person is mapped to is not managed by Orglane (its description lost the Managed by Orglane mark), so Orglane does not change it. Restore that mark in the team's description to keep the team, or rename the team in the app so Orglane creates its own, then retry.",
  },
  forgejo_rejected: {
    id: "apps.code.forgejoRejected",
    defaultMessage: "Forgejo refused the change. Check the details, then retry.",
  },
  account_not_found: {
    id: "apps.code.accountNotFound",
    defaultMessage:
      "The linked account no longer exists in the app. The person links their current account, then Orglane retries.",
  },
  role_protected: {
    id: "apps.code.roleProtected",
    defaultMessage:
      "This person has a special role in the app's organization (for example billing manager), so Orglane never removes them. Change the role in the app if they should go.",
  },
  maintainer_protected: {
    id: "apps.code.maintainerProtected",
    defaultMessage:
      "This person maintains the team in the app, so Orglane never removes them from it. Change their team role in the app if they should go.",
  },
  invite_refused: {
    id: "apps.code.inviteRefused",
    defaultMessage:
      "The app refused to invite this person (for example, the account blocks the organization). Resolve it in the app, then retry.",
  },
  not_invited: {
    id: "apps.code.notInvited",
    defaultMessage:
      "The person is not invited to the app's organization yet. Orglane invites them and retries automatically.",
  },
  login_changed: {
    id: "apps.code.loginChanged",
    defaultMessage:
      "The account's username changed while Orglane worked. Orglane retries automatically.",
  },
  github_unavailable: {
    id: "apps.code.githubUnavailable",
    defaultMessage: "GitHub did not answer. Orglane retries automatically.",
  },
  github_unauthorized: {
    id: "apps.code.githubUnauthorized",
    defaultMessage:
      "GitHub refused the App's access. Orglane retries; if this stays, check that the App is still installed on the organization.",
  },
  github_forbidden: {
    id: "apps.code.githubForbidden",
    defaultMessage:
      "GitHub does not allow the App to make this change (for example, a team synced from an identity provider). Change it on GitHub, then retry.",
  },
  github_rejected: {
    id: "apps.code.githubRejected",
    defaultMessage: "GitHub refused the change. Check the details, then retry.",
  },
});

/** What a person still has to do, by employee step (see onboarding). */
export const stepMessages: Record<string, MessageDescriptor> = defineMessages({
  "github.link-account": {
    id: "apps.step.githubLinkAccount",
    defaultMessage:
      "Waiting for this person to link their GitHub account (a step in their onboarding).",
  },
});

/** The explanation of an employee step, if it is a known one. */
export function explainStep(intl: IntlShape, step: string | undefined): string | undefined {
  const m = step ? stepMessages[step] : undefined;
  return m ? intl.formatMessage(m) : undefined;
}

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
