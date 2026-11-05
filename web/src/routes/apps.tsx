import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowLeft, ExternalLink, LayoutGrid, Plus, RefreshCw } from "lucide-react";
import { useEffect, useId, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { navMessages } from "@/components/app-shell";
import { EmptyState } from "@/components/empty-state";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import {
  type AppInstance,
  type AppProvider,
  actionMessages,
  appBindingsQuery,
  appInstanceQuery,
  appInstancesQuery,
  appOperationsQuery,
  appProvidersQuery,
  appProvisioningQuery,
  appsKey,
  appTeamMappingsQuery,
  explainCode,
  featureMessages,
  guidanceMessages,
  healthMessages,
  label,
  managementMessages,
  myAppsQuery,
  operationFinished,
  operationMessages,
  operationStateMessages,
  operationStateVariant,
  outcomeMessages,
  policyMessages,
  stateMessages,
  stateVariant,
  statusMessages,
  statusVariant,
  supportMessages,
  waitingCodes,
  waitingMessages,
} from "@/lib/apps";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";
import { teamsQuery } from "@/lib/teams";
import { cn } from "@/lib/utils";

const messages = defineMessages({
  description: {
    id: "apps.description",
    defaultMessage: "The services your company uses, with one sign-in.",
  },
  emptyTitle: { id: "apps.empty.title", defaultMessage: "No apps assigned to you yet" },
  emptyDescription: {
    id: "apps.empty.description",
    defaultMessage:
      "Your company's apps appear here once you have access. Ask an administrator if you need one.",
  },
  manageApps: { id: "apps.manage", defaultMessage: "Manage apps" },
  open: { id: "apps.open", defaultMessage: "Open" },
  completeStep: { id: "apps.completeStep", defaultMessage: "Finish setting up" },
  settingsTitle: { id: "appsSettings.title", defaultMessage: "Apps" },
  settingsDescription: {
    id: "appsSettings.description",
    defaultMessage: "Connect your company's tools and decide who gets access.",
  },
  noPermission: {
    id: "appsSettings.noPermission",
    defaultMessage: "You don't have permission to manage apps.",
  },
  instances: { id: "appsSettings.instances", defaultMessage: "Connected apps" },
  noInstances: { id: "appsSettings.noInstances", defaultMessage: "No apps yet" },
  noInstancesDescription: {
    id: "appsSettings.noInstancesDescription",
    defaultMessage:
      "Apps appear here once they are installed or connected. The capability matrix below shows what each available provider can do.",
  },
  providers: { id: "appsSettings.providers", defaultMessage: "Capability matrix" },
  providersDescription: {
    id: "appsSettings.providersDescription",
    defaultMessage: "What each provider compiled into this server supports.",
  },
  noProviders: {
    id: "appsSettings.noProviders",
    defaultMessage: "No providers are available in this version yet.",
  },
  feature: { id: "appsSettings.feature", defaultMessage: "Feature" },
  name: { id: "appsSettings.name", defaultMessage: "Name" },
  provider: { id: "appsSettings.provider", defaultMessage: "Provider" },
  state: { id: "appsSettings.state", defaultMessage: "State" },
  address: { id: "appsSettings.address", defaultMessage: "Address" },
  defaultFor: defineMessage<{ capability: string }>({
    id: "appsSettings.defaultFor",
    defaultMessage: "Default for {capability}",
  }),
  unavailable: {
    id: "appsSettings.unavailable",
    defaultMessage: "Provider not available in this version",
  },
  plannedIn: defineMessage<{ milestone: string }>({
    id: "appsSettings.plannedIn",
    defaultMessage: "Planned ({milestone})",
  }),
  loading: { id: "common.loading", defaultMessage: "Loading" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
  scrollHint: {
    id: "appsSettings.scrollHint",
    defaultMessage: "Scroll sideways to see all columns.",
  },
  applying: {
    id: "appInstance.applying",
    defaultMessage: "Applying changes in the background. This page updates by itself.",
  },
  rotationWaiting: defineMessage<{ version: number }>({
    id: "appInstance.rotationWaiting",
    defaultMessage:
      "Waiting for the provider to confirm version {version}. The current value stays in use, and Orglane keeps retrying automatically.",
  }),
  abandonExplain: {
    id: "appInstance.abandonExplain",
    defaultMessage:
      "If the provider never accepted the new value, cancel the rotation: Orglane re-applies the current value first, then discards the new one.",
  },
  abandon: defineMessage<{ name: string }>({
    id: "appInstance.abandon",
    defaultMessage: "Cancel the rotation of {name} and keep the current value",
  }),
  abandonShort: {
    id: "appInstance.abandonShort",
    defaultMessage: "Cancel rotation and keep current value",
  },
  abandoning: {
    id: "appInstance.abandoning",
    defaultMessage:
      "Cancelling: Orglane is re-applying the current value. The new value is discarded once the provider confirms.",
  },
  details: { id: "appInstance.details", defaultMessage: "Details" },
  back: { id: "appInstance.back", defaultMessage: "All apps" },
  notFound: { id: "appInstance.notFound", defaultMessage: "App not found" },
  overview: { id: "appInstance.overview", defaultMessage: "Overview" },
  management: { id: "appInstance.management", defaultMessage: "Management" },
  health: { id: "appInstance.health", defaultMessage: "Health" },
  version: { id: "appInstance.version", defaultMessage: "Version" },
  webhookUrl: { id: "appInstance.webhookUrl", defaultMessage: "Webhook address" },
  setupFailed: {
    id: "appInstance.setupFailed",
    defaultMessage: "Setting up this app failed. Check the service and try again.",
  },
  retrySetup: { id: "appInstance.retrySetup", defaultMessage: "Retry setup" },
  access: { id: "appInstance.access", defaultMessage: "Access" },
  accessDescription: {
    id: "appInstance.accessDescription",
    defaultMessage: "Choose who gets an account. Changes are applied in the background.",
  },
  policy: { id: "appInstance.policy", defaultMessage: "Who gets access" },
  manageExisting: {
    id: "appInstance.manageExisting",
    defaultMessage: "Manage existing members (they can be removed when they lose access)",
  },
  manageExistingHint: {
    id: "appInstance.manageExistingHint",
    defaultMessage:
      "Off by default: accounts that existed before Orglane are reported, never removed. Run a dry run before turning this on.",
  },
  save: { id: "appInstance.save", defaultMessage: "Save access settings" },
  saved: { id: "appInstance.saved", defaultMessage: "Access settings saved." },
  credentials: { id: "appInstance.credentials", defaultMessage: "Credentials" },
  credentialsDescription: {
    id: "appInstance.credentialsDescription",
    defaultMessage: "Stored encrypted. Values are never shown.",
  },
  credentialVersion: defineMessage<{ version: number }>({
    id: "appInstance.credentialVersion",
    defaultMessage: "Version {version}",
  }),
  rotate: defineMessage<{ name: string }>({
    id: "appInstance.rotate",
    defaultMessage: "Rotate {name}",
  }),
  rotateShort: { id: "appInstance.rotateShort", defaultMessage: "Rotate" },
  rotating: {
    id: "appInstance.rotating",
    defaultMessage:
      "Rotation started. The current value stays in use until the provider confirms the new one.",
  },
  rotationStuck: {
    id: "appInstance.rotationStuck",
    defaultMessage:
      "A credential rotation has been waiting for the provider's confirmation for more than a day.",
  },
  rotationConflict: {
    id: "appInstance.rotationConflict",
    defaultMessage: "A rotation of this credential is already in progress.",
  },
  teamMappings: { id: "appInstance.teamMappings", defaultMessage: "Team mappings" },
  teamMappingsDescription: {
    id: "appInstance.teamMappingsDescription",
    defaultMessage: "Members of a mapped team join the matching team in the app.",
  },
  noMappings: { id: "appInstance.noMappings", defaultMessage: "No teams are mapped yet." },
  teamToMap: { id: "appInstance.teamToMap", defaultMessage: "Team to map" },
  chooseTeam: { id: "appInstance.chooseTeam", defaultMessage: "Choose a team" },
  mapTeam: { id: "appInstance.mapTeam", defaultMessage: "Map team" },
  unmap: defineMessage<{ name: string }>({
    id: "appInstance.unmap",
    defaultMessage: "Remove the mapping of {name}",
  }),
  unmapShort: { id: "appInstance.unmapShort", defaultMessage: "Remove" },
  waitingSync: { id: "appInstance.waitingSync", defaultMessage: "Waiting for sync" },
  archived: { id: "appInstance.archived", defaultMessage: "Archived" },
  provisioning: { id: "appInstance.provisioning", defaultMessage: "People" },
  provisioningDescription: {
    id: "appInstance.provisioningDescription",
    defaultMessage: "Each person's access in this app.",
  },
  counts: defineMessage<{ done: number; queued: number; pending: number; failed: number }>({
    id: "appInstance.counts",
    defaultMessage:
      "{done} up to date · {queued} updating · {pending} need action · {failed} failed",
  }),
  noResults: { id: "appInstance.noResults", defaultMessage: "Nobody has access yet." },
  person: { id: "appInstance.person", defaultMessage: "Person" },
  status: { id: "appInstance.status", defaultMessage: "Status" },
  lastActions: { id: "appInstance.lastActions", defaultMessage: "Last changes" },
  noAccess: { id: "appInstance.noAccess", defaultMessage: "No access" },
  retryFailed: { id: "appInstance.retryFailed", defaultMessage: "Retry failed" },
  retried: defineMessage<{ count: number }>({
    id: "appInstance.retried",
    defaultMessage:
      "{count, plural, =0 {Nothing to retry.} one {# person queued again.} other {# people queued again.}}",
  }),
  dryRun: { id: "appInstance.dryRun", defaultMessage: "Dry run" },
  dryRunDescription: {
    id: "appInstance.dryRunDescription",
    defaultMessage: "What would change now. Nothing is applied.",
  },
  nothingPlanned: { id: "appInstance.nothingPlanned", defaultMessage: "Everything is up to date." },
  removal: { id: "appInstance.removal", defaultMessage: "Removal" },
  held: { id: "appInstance.held", defaultMessage: "Removals waiting for confirmation" },
  heldDescription: defineMessage<{ count: number }>({
    id: "appInstance.heldDescription",
    defaultMessage:
      "{count, plural, one {# removal was} other {# removals were}} held because they would take access from many people at once. Nothing is removed until you confirm.",
  }),
  confirmRemovals: { id: "appInstance.confirmRemovals", defaultMessage: "Confirm removals" },
  confirmed: {
    id: "appInstance.confirmed",
    defaultMessage: "Confirmed. Each removal is checked again before it is applied.",
  },
  batchChanged: {
    id: "appInstance.batchChanged",
    defaultMessage: "The held removals changed. Review the updated list and confirm again.",
  },
  unmanaged: { id: "appInstance.unmanaged", defaultMessage: "Accounts not managed by Orglane" },
  unmanagedDescription: {
    id: "appInstance.unmanagedDescription",
    defaultMessage:
      "These accounts exist in the app but are not linked to anyone Orglane manages. They are never removed automatically.",
  },
  enableManaged: {
    id: "appInstance.enableManaged",
    defaultMessage: "To start the agent, run this on the server where Orglane is installed:",
  },
  operations: { id: "appInstance.operations", defaultMessage: "Operations" },
  operationsDescription: {
    id: "appInstance.operationsDescription",
    defaultMessage: "What the agent did for this app, newest first.",
  },
  noOperations: { id: "appInstance.noOperations", defaultMessage: "No operations yet." },
  operation: { id: "appInstance.operation", defaultMessage: "Operation" },
  generation: defineMessage<{ generation: number }>({
    id: "appInstance.generation",
    defaultMessage: "Change {generation}",
  }),
  started: { id: "appInstance.started", defaultMessage: "Started" },
  finished: { id: "appInstance.finished", defaultMessage: "Finished" },
  versionChange: defineMessage<{ from: string; to: string }>({
    id: "appInstance.versionChange",
    defaultMessage: "{from} → {to}",
  }),
  showMore: { id: "appInstance.showMore", defaultMessage: "Show more" },
});

/** The one-line command that turns on managed services (docs/specs/agent.md). */
const enableManagedCommand = "curl -fsSL https://orglane.app/install.sh | sh -s -- enable-managed";

/** How often the instance page refreshes while background work runs. */
const POLL_MS = 2000;
/** How long after a change the page keeps refreshing, even before any result exists. */
const BUSY_MS = 60_000;

/**
 * Background work (sync, provisioning, rotation) finishes after the change
 * that started it: `mark` keeps the instance page refreshing for a while.
 */
function useBusy() {
  const [until, setUntil] = useState(0);
  const [, rerender] = useState(0);
  useEffect(() => {
    const left = until - Date.now();
    if (left <= 0) return;
    const timer = window.setTimeout(() => rerender((n) => n + 1), left);
    return () => window.clearTimeout(timer);
  }, [until]);
  return { busy: until > Date.now(), mark: () => setUntil(Date.now() + BUSY_MS) };
}

const transitional = new Set(["connecting", "installing", "upgrading", "removing"]);

/** On narrow screens, says that a wide table scrolls sideways. */
function ScrollHint() {
  return (
    <p className="mb-2 text-muted-foreground text-xs sm:hidden">
      <FormattedMessage {...messages.scrollHint} />
    </p>
  );
}

function Loading() {
  const intl = useIntl();
  return (
    <div role="status" aria-label={intl.formatMessage(messages.loading)} className="p-6">
      <Spinner />
    </div>
  );
}

function LoadError({ error, retry }: { error: unknown; retry: () => void }) {
  const intl = useIntl();
  return (
    <Alert>
      <p>{errorMessage(intl, error)}</p>
      <Button variant="link" onClick={retry}>
        <FormattedMessage {...messages.retry} />
      </Button>
    </Alert>
  );
}

function StateBadge({ state }: { state: Schema<"AppInstanceState"> }) {
  const intl = useIntl();
  return <Badge variant={stateVariant(state)}>{intl.formatMessage(stateMessages[state])}</Badge>;
}

function StatusBadge({ status }: { status: Schema<"ProvisioningStatus"> }) {
  const intl = useIntl();
  return (
    <Badge variant={statusVariant(status)}>{intl.formatMessage(statusMessages[status])}</Badge>
  );
}

/** The person's apps (Apps in the main navigation). */
export function AppsPage() {
  const api = useApi();
  const intl = useIntl();
  const manage = useMe().permissions.includes("apps.manage");
  const mine = useQuery(myAppsQuery(api));
  return (
    <Page
      title={intl.formatMessage(navMessages.apps)}
      description={<FormattedMessage {...messages.description} />}
      actions={
        manage && (
          <Button asChild variant="outline">
            <Link to="/settings/apps">
              <FormattedMessage {...messages.manageApps} />
            </Link>
          </Button>
        )
      }
    >
      {mine.isPending ? (
        <Loading />
      ) : mine.isError ? (
        <LoadError error={mine.error} retry={() => void mine.refetch()} />
      ) : mine.data.items.length === 0 ? (
        <EmptyState
          icon={LayoutGrid}
          title={<FormattedMessage {...messages.emptyTitle} />}
          description={<FormattedMessage {...messages.emptyDescription} />}
        />
      ) : (
        <ul className="grid gap-4 lg:grid-cols-2">
          {mine.data.items.map((app) => (
            <li key={app.instanceId}>
              <Card className="flex h-full flex-col gap-4 p-5">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <h2 className="break-words font-semibold">{app.displayName}</h2>
                    <p className="break-all text-muted-foreground text-sm">
                      {app.homeUrl ? new URL(app.homeUrl).host : app.key}
                    </p>
                  </div>
                  <StatusBadge status={app.status} />
                </div>
                {app.status !== "done" && (
                  <p className="text-muted-foreground text-sm">
                    <FormattedMessage
                      {...(app.status === "failed"
                        ? guidanceMessages.myFailed
                        : app.status === "pending_employee_action"
                          ? guidanceMessages.myAction
                          : guidanceMessages.myQueued)}
                    />
                  </p>
                )}
                <div className="mt-auto flex flex-wrap gap-2">
                  {app.status === "pending_employee_action" && app.actionPath ? (
                    <Button asChild size="sm">
                      <a href={app.actionPath}>
                        <FormattedMessage {...messages.completeStep} />
                      </a>
                    </Button>
                  ) : null}
                  {app.homeUrl && (
                    <Button asChild size="sm" variant="outline">
                      <a href={app.homeUrl} target="_blank" rel="noopener noreferrer">
                        <ExternalLink aria-hidden="true" />
                        <FormattedMessage {...messages.open} />
                      </a>
                    </Button>
                  )}
                </div>
              </Card>
            </li>
          ))}
        </ul>
      )}
    </Page>
  );
}

function CapabilityMatrix({ providers }: { providers: AppProvider[] }) {
  const intl = useIntl();
  if (providers.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        <FormattedMessage {...messages.noProviders} />
      </p>
    );
  }
  const features = providers[0]?.features.map((f) => f.feature) ?? [];
  return (
    <div className="overflow-x-auto">
      <table className={cn("w-full text-left text-sm", providers.length > 2 && "min-w-[36rem]")}>
        <caption className="sr-only">{intl.formatMessage(messages.providers)}</caption>
        <thead>
          <tr className="border-b text-muted-foreground">
            <th scope="col" className="py-2 pr-4 font-medium">
              <FormattedMessage {...messages.feature} />
            </th>
            {providers.map((p) => (
              <th key={p.id} scope="col" className="py-2 pr-4 font-medium">
                {p.name}
                <span className="block font-normal text-xs">
                  {intl.formatMessage(managementMessages[p.management])}
                </span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {features.map((feature) => (
            <tr key={feature} className="border-b last:border-0">
              <th scope="row" className="py-2 pr-4 font-normal">
                {label(intl, featureMessages, feature)}
              </th>
              {providers.map((p) => {
                const cell = p.features.find((f) => f.feature === feature);
                if (!cell) return <td key={p.id} className="py-2 pr-4" />;
                const label =
                  cell.support === "planned" && cell.milestone
                    ? intl.formatMessage(messages.plannedIn, { milestone: cell.milestone })
                    : intl.formatMessage(supportMessages[cell.support]);
                return (
                  <td key={p.id} className="py-2 pr-4">
                    <Badge
                      variant={
                        cell.support === "supported"
                          ? "success"
                          : cell.support === "partial"
                            ? "warning"
                            : cell.support === "planned"
                              ? "brand"
                              : "neutral"
                      }
                    >
                      {label}
                    </Badge>
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Settings → Apps: app instances, their state and the capability matrix. */
export function AppsSettingsPage() {
  const api = useApi();
  const intl = useIntl();
  const manage = useMe().permissions.includes("apps.manage");
  const instances = useQuery({ ...appInstancesQuery(api), enabled: manage });
  const providers = useQuery({ ...appProvidersQuery(api), enabled: manage });
  const bindings = useQuery({ ...appBindingsQuery(api), enabled: manage });
  const title = intl.formatMessage(messages.settingsTitle);
  if (!manage) {
    return (
      <Page title={title}>
        <Alert>
          <FormattedMessage {...messages.noPermission} />
        </Alert>
      </Page>
    );
  }
  const providerName = (id: string) => providers.data?.items.find((p) => p.id === id)?.name ?? id;
  const defaults = (instanceId: string) =>
    bindings.data?.items.filter((b) => b.instanceId === instanceId && b.isDefault) ?? [];
  return (
    <Page title={title} description={<FormattedMessage {...messages.settingsDescription} />}>
      <section aria-labelledby="apps-instances" className="mb-8">
        <h2 id="apps-instances" className="mb-3 font-semibold text-lg">
          <FormattedMessage {...messages.instances} />
        </h2>
        {instances.isPending ? (
          <Loading />
        ) : instances.isError ? (
          <LoadError error={instances.error} retry={() => void instances.refetch()} />
        ) : instances.data.items.length === 0 ? (
          <EmptyState
            icon={LayoutGrid}
            title={<FormattedMessage {...messages.noInstances} />}
            description={<FormattedMessage {...messages.noInstancesDescription} />}
          />
        ) : (
          <>
            <ScrollHint />
            <Card className="overflow-x-auto">
              <table className="w-full min-w-[36rem] text-left text-sm">
                <caption className="sr-only">{intl.formatMessage(messages.instances)}</caption>
                <thead>
                  <tr className="border-b text-muted-foreground">
                    <th scope="col" className="sticky left-0 bg-surface px-5 py-3 font-medium">
                      <FormattedMessage {...messages.name} />
                    </th>
                    <th scope="col" className="px-5 py-3 font-medium">
                      <FormattedMessage {...messages.provider} />
                    </th>
                    <th scope="col" className="px-5 py-3 font-medium">
                      <FormattedMessage {...messages.state} />
                    </th>
                    <th scope="col" className="px-5 py-3 font-medium">
                      <FormattedMessage {...messages.address} />
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {instances.data.items.map((inst) => (
                    <tr key={inst.id} className="border-b last:border-0">
                      <td className="sticky left-0 bg-surface px-5 py-3">
                        <Link
                          to="/settings/apps/$instanceId"
                          params={{ instanceId: inst.id }}
                          className="font-medium text-primary hover:underline"
                        >
                          {inst.displayName}
                        </Link>
                        <div className="mt-1 flex flex-wrap gap-1">
                          {defaults(inst.id).map((b) => (
                            <Badge key={b.capability} variant="brand">
                              {intl.formatMessage(messages.defaultFor, {
                                capability: b.capability,
                              })}
                            </Badge>
                          ))}
                        </div>
                      </td>
                      <td className="px-5 py-3">
                        {providerName(inst.provider)}
                        {!inst.providerAvailable && (
                          <span className="block text-muted-foreground text-xs">
                            <FormattedMessage {...messages.unavailable} />
                          </span>
                        )}
                      </td>
                      <td className="px-5 py-3">
                        <div className="flex flex-wrap gap-1">
                          <StateBadge state={inst.state} />
                          {inst.waitingForAgent && (
                            <Badge variant="warning">
                              {label(intl, waitingMessages, inst.waitingForAgent)}
                            </Badge>
                          )}
                          {inst.state !== "removed" && inst.health !== "unknown" && (
                            <Badge variant={inst.health === "healthy" ? "success" : "warning"}>
                              {intl.formatMessage(healthMessages[inst.health])}
                            </Badge>
                          )}
                        </div>
                      </td>
                      <td className="break-all px-5 py-3 text-muted-foreground">
                        {inst.baseUrl ?? inst.key}
                        {inst.packageVersion && (
                          <span className="block text-xs">{inst.packageVersion}</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
          </>
        )}
      </section>
      <section aria-labelledby="apps-matrix">
        <Card>
          <CardHeader>
            <CardTitle id="apps-matrix">
              <FormattedMessage {...messages.providers} />
            </CardTitle>
            <CardDescription>
              <FormattedMessage {...messages.providersDescription} />
            </CardDescription>
          </CardHeader>
          <CardContent>
            {providers.isPending ? (
              <Loading />
            ) : providers.isError ? (
              <LoadError error={providers.error} retry={() => void providers.refetch()} />
            ) : (
              <CapabilityMatrix providers={providers.data.items} />
            )}
          </CardContent>
        </Card>
      </section>
    </Page>
  );
}

function AccessSettings({ instance, onChange }: { instance: AppInstance; onChange: () => void }) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const policyId = useId();
  const [policy, setPolicy] = useState(instance.accessPolicy);
  const [manageExisting, setManageExisting] = useState(instance.manageExistingMembers);
  // Follow changes saved elsewhere without remounting (which would hide the
  // "saved" confirmation).
  useEffect(() => {
    setPolicy(instance.accessPolicy);
    setManageExisting(instance.manageExistingMembers);
  }, [instance.accessPolicy, instance.manageExistingMembers]);
  const save = useMutation({
    mutationFn: () =>
      call(
        api.client.PATCH("/api/v1/apps/instances/{instanceId}", {
          params: { path: { instanceId: instance.id } },
          body: { accessPolicy: policy, manageExistingMembers: manageExisting },
        }),
      ),
    onSuccess: () => {
      onChange();
      return queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...messages.access} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...messages.accessDescription} />
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          {save.isError && <Alert>{errorMessage(intl, save.error)}</Alert>}
          {save.isSuccess && (
            <Alert variant="success">
              <FormattedMessage {...messages.saved} />
            </Alert>
          )}
          <div className="flex flex-col gap-2">
            <Label htmlFor={policyId}>
              <FormattedMessage {...messages.policy} />
            </Label>
            <select
              id={policyId}
              value={policy}
              onChange={(e) => setPolicy(e.target.value as Schema<"AppAccessPolicy">)}
              className="h-10 max-w-sm rounded-md border bg-surface px-3 text-sm sm:h-9"
              disabled={save.isPending}
            >
              {(["all_people", "mapped_teams"] as const).map((p) => (
                <option key={p} value={p}>
                  {intl.formatMessage(policyMessages[p])}
                </option>
              ))}
            </select>
          </div>
          <div className="flex items-start gap-2">
            <input
              id={`${policyId}-existing`}
              type="checkbox"
              className="mt-1 size-4"
              checked={manageExisting}
              onChange={(e) => setManageExisting(e.target.checked)}
              disabled={save.isPending}
              aria-describedby={`${policyId}-existing-hint`}
            />
            <div>
              <Label htmlFor={`${policyId}-existing`}>
                <FormattedMessage {...messages.manageExisting} />
              </Label>
              <p id={`${policyId}-existing-hint`} className="mt-1 text-muted-foreground text-sm">
                <FormattedMessage {...messages.manageExistingHint} />
              </p>
            </div>
          </div>
          <Button type="submit" loading={save.isPending}>
            <FormattedMessage {...messages.save} />
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

function Credentials({
  instanceId,
  provider,
  detail,
  onChange,
}: {
  instanceId: string;
  provider: AppProvider | undefined;
  detail: Schema<"AppInstanceDetail">;
  onChange: () => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const rotate = useMutation({
    mutationFn: (credentialName: string) =>
      call(
        api.client.POST("/api/v1/apps/instances/{instanceId}/credentials/{credentialName}/rotate", {
          params: { path: { instanceId, credentialName } },
        }),
      ),
    onSuccess: () => {
      onChange();
      return queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });
  const abandon = useMutation({
    mutationFn: (credentialName: string) =>
      call(
        api.client.POST(
          "/api/v1/apps/instances/{instanceId}/credentials/{credentialName}/abandon",
          {
            params: { path: { instanceId, credentialName } },
          },
        ),
      ),
    onSuccess: () => {
      onChange();
      return queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });
  const rotatable = (name: string) =>
    provider?.credentials.some(
      (c) => c.name === name && c.rotatable && !c.agent && c.source !== "upload",
    );
  const active = detail.credentials.filter((c) => c.state === "active");
  if (active.length === 0) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...messages.credentials} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...messages.credentialsDescription} />
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {detail.rotationStuck && (
          <Alert>
            <FormattedMessage {...messages.rotationStuck} />
          </Alert>
        )}
        {rotate.isError && (
          <Alert>{errorMessage(intl, rotate.error, { 409: messages.rotationConflict })}</Alert>
        )}
        {abandon.isError && <Alert>{errorMessage(intl, abandon.error)}</Alert>}
        {rotate.isSuccess && (
          <Alert variant="success">
            <FormattedMessage {...messages.rotating} />
          </Alert>
        )}
        <ul className="divide-y">
          {active.map((c) => {
            const candidate = detail.credentials.find(
              (o) =>
                o.name === c.name &&
                (o.state === "pending" || o.state === "uncertain" || o.state === "abandoning"),
            );
            return (
              <li key={c.name} className="space-y-2 py-3 text-sm">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <span>
                    <code className="font-mono">{c.name}</code>
                    <span className="ml-2 text-muted-foreground">
                      {intl.formatMessage(messages.credentialVersion, { version: c.version })}
                    </span>
                  </span>
                  {rotatable(c.name) && !candidate && (
                    <Button
                      size="sm"
                      variant="outline"
                      aria-label={intl.formatMessage(messages.rotate, { name: c.name })}
                      disabled={rotate.isPending}
                      onClick={() => rotate.mutate(c.name)}
                    >
                      <RefreshCw aria-hidden="true" />
                      <FormattedMessage {...messages.rotateShort} />
                    </Button>
                  )}
                </div>
                {candidate?.state === "abandoning" && (
                  <p role="status" className="text-muted-foreground">
                    <FormattedMessage {...messages.abandoning} />
                  </p>
                )}
                {candidate && candidate.state !== "abandoning" && (
                  <div className="space-y-2 rounded-lg border p-3">
                    <p role="status">
                      {intl.formatMessage(messages.rotationWaiting, { version: candidate.version })}
                    </p>
                    <p className="text-muted-foreground">
                      <FormattedMessage {...messages.abandonExplain} />
                    </p>
                    <Button
                      size="sm"
                      variant="outline"
                      aria-label={intl.formatMessage(messages.abandon, { name: c.name })}
                      loading={abandon.isPending}
                      onClick={() => abandon.mutate(c.name)}
                    >
                      <FormattedMessage {...messages.abandonShort} />
                    </Button>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      </CardContent>
    </Card>
  );
}

function TeamMappings({
  instanceId,
  busy,
  onChange,
}: {
  instanceId: string;
  busy: boolean;
  onChange: () => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const selectId = useId();
  const [teamId, setTeamId] = useState("");
  const mappings = useQuery({
    ...appTeamMappingsQuery(api, instanceId),
    // Until the provider team exists, the mapping is "waiting for sync".
    refetchInterval: (q) =>
      busy || q.state.data?.items.some((m) => !m.externalRef && !m.teamArchived) ? POLL_MS : false,
  });
  const teams = useQuery(teamsQuery(api));
  const invalidate = () => {
    onChange();
    return queryClient.invalidateQueries({ queryKey: appsKey });
  };
  const add = useMutation({
    mutationFn: () =>
      call(
        api.client.PUT("/api/v1/apps/instances/{instanceId}/team-mappings/{teamId}", {
          params: { path: { instanceId, teamId } },
          body: { mode: "create" },
        }),
      ),
    onSuccess: async () => {
      setTeamId("");
      await invalidate();
    },
  });
  const remove = useMutation({
    mutationFn: (id: string) =>
      call(
        api.client.DELETE("/api/v1/apps/instances/{instanceId}/team-mappings/{teamId}", {
          params: { path: { instanceId, teamId: id } },
        }),
      ),
    onSuccess: invalidate,
  });
  const available =
    teams.data?.items.filter((t) => !mappings.data?.items.some((m) => m.teamId === t.id)) ?? [];
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...messages.teamMappings} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...messages.teamMappingsDescription} />
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {(add.isError || remove.isError) && (
          <Alert>{errorMessage(intl, add.error ?? remove.error)}</Alert>
        )}
        {mappings.isPending ? (
          <Loading />
        ) : mappings.isError ? (
          <LoadError error={mappings.error} retry={() => void mappings.refetch()} />
        ) : mappings.data.items.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            <FormattedMessage {...messages.noMappings} />
          </p>
        ) : (
          <ul className="divide-y">
            {mappings.data.items.map((m) => (
              <li key={m.teamId} className="flex items-center justify-between gap-3 py-2 text-sm">
                <span className="min-w-0">
                  <span className="font-medium">{m.teamName}</span>
                  <span className="ml-2 text-muted-foreground">
                    {m.externalName ?? <FormattedMessage {...messages.waitingSync} />}
                  </span>
                  {m.teamArchived && (
                    <Badge className="ml-2">
                      <FormattedMessage {...messages.archived} />
                    </Badge>
                  )}
                </span>
                <Button
                  size="sm"
                  variant="destructive-ghost"
                  aria-label={intl.formatMessage(messages.unmap, { name: m.teamName })}
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(m.teamId)}
                >
                  <FormattedMessage {...messages.unmapShort} />
                </Button>
              </li>
            ))}
          </ul>
        )}
        {available.length > 0 && (
          <form
            className="flex flex-wrap items-end gap-3 border-t pt-4"
            onSubmit={(e) => {
              e.preventDefault();
              if (teamId) add.mutate();
            }}
          >
            <div className="flex min-w-0 flex-1 flex-col gap-2">
              <Label htmlFor={selectId}>
                <FormattedMessage {...messages.teamToMap} />
              </Label>
              <select
                id={selectId}
                required
                value={teamId}
                onChange={(e) => setTeamId(e.target.value)}
                className="h-10 min-w-0 rounded-md border bg-surface px-3 text-sm sm:h-9"
                disabled={add.isPending}
              >
                <option value="">{intl.formatMessage(messages.chooseTeam)}</option>
                {available.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
              </select>
            </div>
            <Button type="submit" loading={add.isPending} disabled={!teamId}>
              <Plus aria-hidden="true" />
              <FormattedMessage {...messages.mapTeam} />
            </Button>
          </form>
        )}
      </CardContent>
    </Card>
  );
}

function ActionList({
  actions,
}: {
  actions: { kind: string; key: string; removal: boolean; outcome?: string }[];
}) {
  const intl = useIntl();
  if (actions.length === 0) return null;
  return (
    <ul className="space-y-0.5 text-muted-foreground text-xs">
      {actions.map((a) => (
        <li key={a.key}>
          {label(intl, actionMessages, a.kind)}
          {a.outcome && ` · ${label(intl, outcomeMessages, a.outcome)}`}
        </li>
      ))}
    </ul>
  );
}

function Provisioning({
  instanceId,
  busy,
  onChange,
}: {
  instanceId: string;
  busy: boolean;
  onChange: () => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const status = useQuery({
    ...appProvisioningQuery(api, instanceId),
    refetchInterval: (q) => (busy || (q.state.data?.counts.queued ?? 0) > 0 ? POLL_MS : false),
  });
  const invalidate = () => {
    onChange();
    return queryClient.invalidateQueries({ queryKey: appsKey });
  };
  const retry = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/apps/instances/{instanceId}/provisioning/retry", {
          params: { path: { instanceId } },
          body: {},
        }),
      ),
    onSuccess: invalidate,
  });
  const plan = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/apps/instances/{instanceId}/provisioning/plan", {
          params: { path: { instanceId } },
        }),
      ),
  });
  const confirm = useMutation({
    mutationFn: (batch: Schema<"HeldRemovalBatch">) =>
      call(
        api.client.POST(
          "/api/v1/apps/instances/{instanceId}/provisioning/removal-batches/{batchId}/confirm",
          {
            params: { path: { instanceId, batchId: batch.id } },
            body: { itemsHash: batch.itemsHash },
          },
        ),
      ),
    onSettled: invalidate,
  });
  if (status.isPending) return <Loading />;
  if (status.isError) return <LoadError error={status.error} retry={() => void status.refetch()} />;
  const { counts, items, heldRemovals, unmanagedAccounts } = status.data;
  const changes = plan.data?.items.filter(
    (p) => p.actions.length > 0 || p.employeeStep || p.errorCode,
  );
  return (
    <div className="space-y-5">
      {/* Outside the batch card: after confirming, the batch is no longer held. */}
      {confirm.isError && (
        <Alert>{errorMessage(intl, confirm.error, { 409: messages.batchChanged })}</Alert>
      )}
      {confirm.isSuccess && (
        <Alert variant="success" role="status">
          <FormattedMessage {...messages.confirmed} />
        </Alert>
      )}
      {heldRemovals && (
        <Card className="border-warning">
          <CardHeader>
            <CardTitle>
              <FormattedMessage {...messages.held} />
            </CardTitle>
            <CardDescription>
              {intl.formatMessage(messages.heldDescription, { count: heldRemovals.itemCount })}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <ul className="divide-y text-sm">
              {heldRemovals.items.map((it) => (
                <li key={it.id} className="flex flex-wrap justify-between gap-2 py-2">
                  <span className="font-medium">{it.person.displayName}</span>
                  <span className="text-muted-foreground">
                    {label(intl, actionMessages, it.kind)}
                  </span>
                </li>
              ))}
            </ul>
            <Button
              variant="destructive"
              loading={confirm.isPending}
              onClick={() => confirm.mutate(heldRemovals)}
            >
              <FormattedMessage {...messages.confirmRemovals} />
            </Button>
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader className="sm:flex-row sm:items-start sm:justify-between">
          <div className="space-y-1">
            <CardTitle>
              <FormattedMessage {...messages.provisioning} />
            </CardTitle>
            <CardDescription>
              {intl.formatMessage(messages.counts, {
                done: counts.done,
                queued: counts.queued,
                pending: counts.pendingEmployeeAction,
                failed: counts.failed,
              })}
            </CardDescription>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              loading={plan.isPending}
              onClick={() => plan.mutate()}
            >
              <FormattedMessage {...messages.dryRun} />
            </Button>
            {counts.failed > 0 && (
              <Button
                size="sm"
                variant="outline"
                loading={retry.isPending}
                onClick={() => retry.mutate()}
              >
                <FormattedMessage {...messages.retryFailed} />
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          {(retry.isError || plan.isError) && (
            <Alert>{errorMessage(intl, retry.error ?? plan.error)}</Alert>
          )}
          {retry.isSuccess && (
            <Alert variant="success" role="status">
              {intl.formatMessage(messages.retried, { count: retry.data.queued })}
            </Alert>
          )}
          {changes && (
            <section
              aria-label={intl.formatMessage(messages.dryRun)}
              className="rounded-lg border p-4"
            >
              <h3 className="font-medium text-sm">
                <FormattedMessage {...messages.dryRun} />
              </h3>
              <p className="mb-2 text-muted-foreground text-xs">
                <FormattedMessage {...messages.dryRunDescription} />
              </p>
              {changes.length === 0 ? (
                <p className="text-sm">
                  <FormattedMessage {...messages.nothingPlanned} />
                </p>
              ) : (
                <ul className="divide-y text-sm">
                  {changes.map((p) => (
                    <li key={p.person.id} className="py-2">
                      <span className="font-medium">{p.person.displayName}</span>
                      <ActionList actions={p.actions} />
                    </li>
                  ))}
                </ul>
              )}
            </section>
          )}
          {(busy || counts.queued > 0) && (
            <p role="status" className="flex items-center gap-2 text-muted-foreground text-sm">
              <Spinner />
              <FormattedMessage {...messages.applying} />
            </p>
          )}
          {items.length === 0 ? (
            !busy && (
              <p className="text-muted-foreground text-sm">
                <FormattedMessage {...messages.noResults} />
              </p>
            )
          ) : (
            <div>
              <ScrollHint />
              <div className="overflow-x-auto">
                <table className="w-full min-w-[30rem] text-left text-sm">
                  <caption className="sr-only">{intl.formatMessage(messages.provisioning)}</caption>
                  <thead>
                    <tr className="border-b text-muted-foreground">
                      <th scope="col" className="sticky left-0 bg-surface py-2 pr-4 font-medium">
                        <FormattedMessage {...messages.person} />
                      </th>
                      <th scope="col" className="py-2 pr-4 font-medium">
                        <FormattedMessage {...messages.status} />
                      </th>
                      <th scope="col" className="py-2 font-medium">
                        <FormattedMessage {...messages.lastActions} />
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((r) => (
                      <tr key={r.person.id} className="border-b align-top last:border-0">
                        <td className="sticky left-0 bg-surface py-2 pr-4">
                          <Link
                            to="/people/$personId"
                            params={{ personId: r.person.id }}
                            className="font-medium text-primary hover:underline"
                          >
                            {r.person.displayName}
                          </Link>
                          {!r.access && (
                            <span className="block text-muted-foreground text-xs">
                              <FormattedMessage {...messages.noAccess} />
                            </span>
                          )}
                        </td>
                        <td className="py-2 pr-4">
                          <StatusBadge status={r.status} />
                          {r.status === "failed" && (
                            <p className="mt-1 max-w-xs text-xs">
                              {explainCode(intl, r.errorCode) ??
                                intl.formatMessage(guidanceMessages.resultFailed)}
                            </p>
                          )}
                          {r.errorCode && (
                            <code className="mt-1 block font-mono text-muted-foreground text-xs">
                              {r.errorCode}
                            </code>
                          )}
                        </td>
                        <td className="py-2">
                          <ActionList actions={r.actions} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
      {unmanagedAccounts.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>
              <FormattedMessage {...messages.unmanaged} />
            </CardTitle>
            <CardDescription>
              <FormattedMessage {...messages.unmanagedDescription} />
            </CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="flex flex-wrap gap-2">
              {unmanagedAccounts.map((a) => (
                <li key={a.externalId}>
                  <Badge>{a.login}</Badge>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

/** Settings → Apps → one app instance. */
/** Why a change waits for the agent, and for a missing agent how to start it. */
function WaitingForAgent({ code }: { code: string }) {
  const intl = useIntl();
  return (
    <Alert>
      <p className="font-medium">{label(intl, waitingMessages, code)}</p>
      <p>{explainCode(intl, code)}</p>
      {code === "agent_unavailable" && (
        <>
          <p className="mt-2">
            <FormattedMessage {...messages.enableManaged} />
          </p>
          <pre className="mt-1 overflow-x-auto rounded-md bg-muted p-2 font-mono text-xs">
            <code>{enableManagedCommand}</code>
          </pre>
        </>
      )}
    </Alert>
  );
}

/** The agent operations of a managed instance (install, changes, upgrades). */
function Operations({ instanceId, working }: { instanceId: string; working: boolean }) {
  const api = useApi();
  const intl = useIntl();
  const operations = useInfiniteQuery({
    ...appOperationsQuery(api, instanceId),
    refetchInterval: (q) =>
      working || q.state.data?.pages[0]?.items.some((op) => !operationFinished(op))
        ? POLL_MS
        : false,
  });
  // Operations recorded while paging shift the offsets: show each once.
  const items = [
    ...new Map(
      (operations.data?.pages ?? []).flatMap((p) => p.items).map((op) => [op.id, op]),
    ).values(),
  ];
  const when = (date: string | undefined) =>
    date ? intl.formatDate(date, { dateStyle: "medium", timeStyle: "short" }) : "—";
  return (
    <Card>
      <CardHeader>
        <CardTitle id="app-operations">
          <FormattedMessage {...messages.operations} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...messages.operationsDescription} />
        </CardDescription>
      </CardHeader>
      <CardContent>
        {operations.isPending ? (
          <Loading />
        ) : operations.isError ? (
          <LoadError error={operations.error} retry={() => void operations.refetch()} />
        ) : items.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            <FormattedMessage {...messages.noOperations} />
          </p>
        ) : (
          <>
            <ScrollHint />
            <div className="overflow-x-auto">
              <table
                className="w-full min-w-[40rem] text-left text-sm"
                aria-labelledby="app-operations"
              >
                <thead>
                  <tr className="border-b text-muted-foreground">
                    <th scope="col" className="py-2 pr-4 font-medium">
                      <FormattedMessage {...messages.operation} />
                    </th>
                    <th scope="col" className="py-2 pr-4 font-medium">
                      <FormattedMessage {...messages.version} />
                    </th>
                    <th scope="col" className="py-2 pr-4 font-medium">
                      <FormattedMessage {...messages.state} />
                    </th>
                    <th scope="col" className="py-2 pr-4 font-medium">
                      <FormattedMessage {...messages.started} />
                    </th>
                    <th scope="col" className="py-2 font-medium">
                      <FormattedMessage {...messages.finished} />
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((op) => {
                    const waiting =
                      !operationFinished(op) && op.errorCode && waitingCodes.has(op.errorCode)
                        ? op.errorCode
                        : undefined;
                    const failure = operationFinished(op) ? op.errorCode : undefined;
                    return (
                      <tr key={op.id} className="border-b align-top last:border-0">
                        <th scope="row" className="py-3 pr-4 font-normal">
                          <span className="font-medium">
                            {label(intl, operationMessages, op.reason ?? op.kind)}
                          </span>
                          {op.generation !== undefined && (
                            <span className="block text-muted-foreground text-xs">
                              {intl.formatMessage(messages.generation, {
                                generation: op.generation,
                              })}
                            </span>
                          )}
                        </th>
                        <td className="py-3 pr-4">
                          {op.fromVersion && op.toVersion && op.fromVersion !== op.toVersion
                            ? intl.formatMessage(messages.versionChange, {
                                from: op.fromVersion,
                                to: op.toVersion,
                              })
                            : (op.toVersion ?? "—")}
                        </td>
                        <td className="py-3 pr-4">
                          <Badge variant={operationStateVariant(op)}>
                            {waiting
                              ? label(intl, waitingMessages, waiting)
                              : intl.formatMessage(operationStateMessages[op.state])}
                          </Badge>
                          {op.phase && !operationFinished(op) && (
                            <span className="block text-muted-foreground text-xs">{op.phase}</span>
                          )}
                          {failure && (
                            <span className="mt-1 block text-muted-foreground text-xs">
                              {explainCode(intl, failure)}{" "}
                              <code className="font-mono">{failure}</code>
                            </span>
                          )}
                        </td>
                        <td className="py-3 pr-4 whitespace-nowrap">{when(op.createdAt)}</td>
                        <td className="py-3 whitespace-nowrap">{when(op.finishedAt)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
            {operations.hasNextPage && (
              <Button
                variant="outline"
                size="sm"
                className="mt-3"
                loading={operations.isFetchingNextPage}
                onClick={() => void operations.fetchNextPage()}
              >
                <FormattedMessage {...messages.showMore} />
              </Button>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

export function AppInstancePage({ instanceId }: { instanceId: string }) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const manage = useMe().permissions.includes("apps.manage");
  const { busy, mark } = useBusy();
  const detail = useQuery({
    ...appInstanceQuery(api, instanceId),
    enabled: manage,
    refetchInterval: (q) => {
      const d = q.state.data;
      const working =
        busy ||
        (d &&
          (transitional.has(d.instance.state) ||
            d.instance.waitingForAgent ||
            d.provisioning.queued > 0 ||
            d.credentials.some(
              (c) => c.state === "pending" || c.state === "uncertain" || c.state === "abandoning",
            )));
      return working ? POLL_MS : false;
    },
  });
  const providers = useQuery({ ...appProvidersQuery(api), enabled: manage });
  const setup = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/apps/instances/{instanceId}/setup", {
          params: { path: { instanceId } },
        }),
      ),
    onSuccess: () => {
      mark();
      return queryClient.invalidateQueries({ queryKey: appsKey });
    },
  });
  const back = (
    <Button asChild variant="ghost" size="sm">
      <Link to="/settings/apps">
        <ArrowLeft aria-hidden="true" />
        <FormattedMessage {...messages.back} />
      </Link>
    </Button>
  );
  if (!manage) {
    return (
      <Page title={intl.formatMessage(messages.settingsTitle)}>
        <Alert>
          <FormattedMessage {...messages.noPermission} />
        </Alert>
      </Page>
    );
  }
  if (detail.isPending) {
    return (
      <Page title={intl.formatMessage(messages.settingsTitle)} actions={back}>
        <Loading />
      </Page>
    );
  }
  if (detail.isError) {
    return (
      <Page title={intl.formatMessage(messages.notFound)} actions={back}>
        <LoadError error={detail.error} retry={() => void detail.refetch()} />
      </Page>
    );
  }
  const { instance } = detail.data;
  const provider = providers.data?.items.find((p) => p.id === instance.provider);
  const provisioningEnabled = provider?.provisioning ?? false;
  return (
    <Page
      title={instance.displayName}
      description={provider?.name ?? instance.provider}
      actions={back}
    >
      <div className="space-y-5">
        <Card>
          <CardHeader>
            <CardTitle>
              <FormattedMessage {...messages.overview} />
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            {instance.state === "failed" && instance.healthCode === "setup_failed" && (
              <Alert>
                <p>
                  <FormattedMessage {...messages.setupFailed} />
                </p>
                <Button variant="link" disabled={setup.isPending} onClick={() => setup.mutate()}>
                  <FormattedMessage {...messages.retrySetup} />
                </Button>
              </Alert>
            )}
            {setup.isError && <Alert>{errorMessage(intl, setup.error)}</Alert>}
            {instance.waitingForAgent && <WaitingForAgent code={instance.waitingForAgent} />}
            {(instance.state === "degraded" ||
              instance.state === "disconnected" ||
              (instance.state === "failed" && instance.healthCode !== "setup_failed")) && (
              <Alert>
                {explainCode(intl, instance.healthCode) ??
                  intl.formatMessage(guidanceMessages.instanceProblem)}
              </Alert>
            )}
            <dl className="grid gap-4 text-sm sm:grid-cols-2">
              <div>
                <dt className="text-muted-foreground">
                  <FormattedMessage {...messages.state} />
                </dt>
                <dd className="mt-1 flex flex-wrap items-center gap-1">
                  <StateBadge state={instance.state} />
                  {instance.healthCode && (
                    <code className="font-mono text-muted-foreground text-xs">
                      {instance.healthCode}
                    </code>
                  )}
                </dd>
              </div>
              <div>
                <dt className="text-muted-foreground">
                  <FormattedMessage {...messages.health} />
                </dt>
                <dd className="mt-1">{intl.formatMessage(healthMessages[instance.health])}</dd>
              </div>
              <div>
                <dt className="text-muted-foreground">
                  <FormattedMessage {...messages.management} />
                </dt>
                <dd className="mt-1">
                  {intl.formatMessage(managementMessages[instance.management])}
                </dd>
              </div>
              <div className="min-w-0">
                <dt className="text-muted-foreground">
                  <FormattedMessage {...messages.address} />
                </dt>
                <dd className="mt-1 break-all">
                  {instance.baseUrl ? (
                    <a
                      href={instance.baseUrl}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-primary hover:underline"
                    >
                      {instance.baseUrl}
                    </a>
                  ) : (
                    instance.key
                  )}
                </dd>
              </div>
              {instance.packageVersion && (
                <div>
                  <dt className="text-muted-foreground">
                    <FormattedMessage {...messages.version} />
                  </dt>
                  <dd className="mt-1">{instance.packageVersion}</dd>
                </div>
              )}
              {instance.webhookUrl && (
                <div className="min-w-0 sm:col-span-2">
                  <dt className="text-muted-foreground">
                    <FormattedMessage {...messages.webhookUrl} />
                  </dt>
                  <dd className="mt-1 break-all font-mono text-xs">{instance.webhookUrl}</dd>
                </div>
              )}
              {detail.data.bindings.some((b) => b.isDefault) && (
                <div className="sm:col-span-2">
                  <dd className="flex flex-wrap gap-1">
                    {detail.data.bindings
                      .filter((b) => b.isDefault)
                      .map((b) => (
                        <Badge key={b.capability} variant="brand">
                          {intl.formatMessage(messages.defaultFor, { capability: b.capability })}
                        </Badge>
                      ))}
                  </dd>
                </div>
              )}
            </dl>
          </CardContent>
        </Card>
        {instance.management === "managed" && (
          <Operations instanceId={instanceId} working={transitional.has(instance.state)} />
        )}
        {instance.state !== "removed" && provisioningEnabled && (
          <>
            <AccessSettings instance={instance} onChange={mark} />
            <TeamMappings instanceId={instanceId} busy={busy} onChange={mark} />
            <Provisioning instanceId={instanceId} busy={busy} onChange={mark} />
          </>
        )}
        <Credentials
          instanceId={instanceId}
          provider={provider}
          detail={detail.data}
          onChange={mark}
        />
      </div>
    </Page>
  );
}
