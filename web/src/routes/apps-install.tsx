import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, Download } from "lucide-react";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { ApiError, call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import {
  type AppAgentStatus,
  type AppInstance,
  type AppProvider,
  appAgentQuery,
  appProvidersQuery,
  appsKey,
  explainCode,
  formatBytes,
  label,
  settingMessages,
  waitingMessages,
} from "@/lib/apps";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";

// Installing, removing, purging and reinstalling managed apps (#61,
// docs/specs/providers.md "Managed instance lifecycle in the core").

const messages = defineMessages({
  installTitle: defineMessage<{ name: string }>({
    id: "appInstall.title",
    defaultMessage: "Install {name}",
  }),
  installDescription: {
    id: "appInstall.description",
    defaultMessage:
      "Orglane runs it on this server, keeps it up to date and signs people in to it.",
  },
  installApps: { id: "appInstall.section", defaultMessage: "Install an app" },
  installAppsDescription: {
    id: "appInstall.sectionDescription",
    defaultMessage: "Apps Orglane can run on this server for you.",
  },
  install: { id: "appInstall.submit", defaultMessage: "Install" },
  back: { id: "appInstall.back", defaultMessage: "Back to apps" },
  loading: { id: "appInstall.loading", defaultMessage: "Loading" },
  noPermission: {
    id: "appInstall.noPermission",
    defaultMessage: "You don't have permission to manage apps.",
  },
  notInstallable: {
    id: "appInstall.notInstallable",
    defaultMessage: "This app cannot be installed on this server.",
  },
  agentMissing: {
    id: "appInstall.agentMissing",
    defaultMessage:
      "Apps run by Orglane need the agent, which is not running. To start it, run this on the server where Orglane is installed:",
  },
  agentWaiting: {
    id: "appInstall.agentWaiting",
    defaultMessage: "This page updates as soon as the agent answers.",
  },
  displayName: { id: "appInstall.displayName", defaultMessage: "Name" },
  displayNameHint: {
    id: "appInstall.displayNameHint",
    defaultMessage: "What people see in Orglane.",
  },
  key: { id: "appInstall.key", defaultMessage: "Key" },
  keyHint: {
    id: "appInstall.keyHint",
    defaultMessage: "A short ID for its data and database. It cannot be changed later.",
  },
  keyInvalid: {
    id: "appInstall.keyInvalid",
    defaultMessage:
      "Use 2 to 24 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen.",
  },
  host: { id: "appInstall.host", defaultMessage: "Address" },
  hostHint: {
    id: "appInstall.hostHint",
    defaultMessage: "Where people open it. It must point to this server, like the workspace.",
  },
  hostInvalid: {
    id: "appInstall.hostInvalid",
    defaultMessage: "Enter a host name such as git.acme.com, without http:// or a path.",
  },
  hostIsWorkspace: defineMessage<{ workspace: string }>({
    id: "appInstall.hostIsWorkspace",
    defaultMessage:
      "{workspace} is the workspace's own address. Give the app an address of its own on the same domain, such as git.{workspace}.",
  }),
  required: { id: "appInstall.required", defaultMessage: "Required." },
  numberRange: defineMessage<{ min: number; max: number }>({
    id: "appInstall.numberRange",
    defaultMessage: "Enter a whole number from {min} to {max}.",
  }),
  patternInvalid: {
    id: "appInstall.patternInvalid",
    defaultMessage: "This value is not accepted here.",
  },
  serverRefused: {
    id: "appInstall.serverRefused",
    defaultMessage: "This value was not accepted. Check it and try again.",
  },
  resources: { id: "appInstall.resources", defaultMessage: "Resources" },
  needs: defineMessage<{ min: string; recommended: string; disk: string }>({
    id: "appInstall.needs",
    defaultMessage:
      "Needs at least {min} of memory ({recommended} recommended) and {disk} of free disk.",
  }),
  server: defineMessage<{ free: string; total: string; disk: string }>({
    id: "appInstall.server",
    defaultMessage:
      "This server: {free} of {total} memory not taken by Orglane and its apps, {disk} of free disk.",
  }),
  tooLittle: {
    id: "appInstall.tooLittle",
    defaultMessage: "This server does not have enough memory or disk for this app.",
  },
  belowRecommended: {
    id: "appInstall.belowRecommended",
    defaultMessage:
      "Less memory is left than recommended. The app runs, but may be slow when many people use it.",
  },
  // Lifecycle of an instance.
  lifecycle: { id: "appLifecycle.title", defaultMessage: "Remove or reinstall" },
  remove: { id: "appLifecycle.remove", defaultMessage: "Remove" },
  removeExplain: {
    id: "appLifecycle.removeExplain",
    defaultMessage:
      "Stops the app and removes its services from this server. Its data, database and settings are kept, so you can reinstall it with everything in it. Until then nobody can use it.",
  },
  confirmRemove: { id: "appLifecycle.confirmRemove", defaultMessage: "Remove the app" },
  purge: { id: "appLifecycle.purge", defaultMessage: "Delete permanently" },
  purgeExplain: {
    id: "appLifecycle.purgeExplain",
    defaultMessage:
      "Removes the app and deletes all its data: repositories, accounts, its database, its credentials and its Orglane sign-in. This cannot be undone.",
  },
  purgeType: defineMessage<{ key: string }>({
    id: "appLifecycle.purgeType",
    defaultMessage: "To confirm, type the app's key {key}",
  }),
  confirmPurge: { id: "appLifecycle.confirmPurge", defaultMessage: "Delete all data" },
  cancel: { id: "appLifecycle.cancel", defaultMessage: "Cancel" },
  reinstall: { id: "appLifecycle.reinstall", defaultMessage: "Reinstall" },
  reinstallExplain: {
    id: "appLifecycle.reinstallExplain",
    defaultMessage:
      "This app was removed and its data kept. Reinstalling brings it back with its data.",
  },
  purgeFailed: {
    id: "appLifecycle.purgeFailed",
    defaultMessage:
      "Deleting this app's data did not finish and may have deleted part of it, so it can only be deleted permanently again.",
  },
  removing: {
    id: "appLifecycle.removing",
    defaultMessage: "The app is being removed. The operations below show the progress.",
  },
  purged: {
    id: "appLifecycle.purged",
    defaultMessage: "This app was deleted with all its data. Its key and address are free again.",
  },
});

/** The one-line command that turns on managed services (docs/specs/agent.md). */
export const enableManagedCommand =
  "curl -fsSL https://orglane.app/install.sh | sh -s -- enable-managed";

/** The command that starts the agent, for a shell on the server. */
export function EnableManagedCommand() {
  return (
    <pre className="mt-1 overflow-x-auto rounded-md bg-muted p-2 font-mono text-xs">
      <code>{enableManagedCommand}</code>
    </pre>
  );
}

/** Why apps cannot be installed now: the agent is missing or not usable. */
function AgentProblem({ agent }: { agent: AppAgentStatus }) {
  const intl = useIntl();
  const code = agent.code ?? "agent_unavailable";
  return (
    <Alert variant="info">
      <div className="min-w-0">
        <p className="font-medium">{label(intl, waitingMessages, code)}</p>
        {code === "agent_unavailable" ? (
          <>
            <p className="mt-1">
              <FormattedMessage {...messages.agentMissing} />
            </p>
            <EnableManagedCommand />
          </>
        ) : (
          <p className="mt-1">{explainCode(intl, code)}</p>
        )}
        <p className="mt-2 text-xs">
          <FormattedMessage {...messages.agentWaiting} />
        </p>
      </div>
    </Alert>
  );
}

/** The managed apps this server can install, on Settings → Apps. */
export function InstallableApps({ providers }: { providers: AppProvider[] }) {
  const intl = useIntl();
  const installable = providers.filter((p) => p.install);
  if (installable.length === 0) return null;
  return (
    <section aria-labelledby="apps-install" className="mb-8">
      <Card>
        <CardHeader>
          <CardTitle id="apps-install">
            <FormattedMessage {...messages.installApps} />
          </CardTitle>
          <CardDescription>
            <FormattedMessage {...messages.installAppsDescription} />
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ul className="flex flex-wrap gap-2">
            {installable.map((p) => (
              <li key={p.id}>
                <Button asChild variant="outline">
                  <Link to="/settings/apps/install/$providerId" params={{ providerId: p.id }}>
                    <Download aria-hidden="true" />
                    {intl.formatMessage(messages.installTitle, { name: p.name })}
                  </Link>
                </Button>
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>
    </section>
  );
}

const keyPattern = /^[a-z][a-z0-9-]{0,22}[a-z0-9]$/;
const hostPattern =
  /^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;

type SettingSpec = Schema<"AppSettingSpec">;

/** The value of a setting from its input, or an error message. */
function settingValue(
  spec: SettingSpec,
  raw: string | boolean,
): { value: unknown } | { error: "required" | "range" | "pattern" } {
  if (spec.type === "bool") return { value: raw === true };
  const text = String(raw).trim();
  if (text === "") return spec.required ? { error: "required" } : { value: undefined };
  if (spec.type === "int") {
    const n = Number(text);
    if (
      !Number.isInteger(n) ||
      (spec.min !== undefined && n < spec.min) ||
      (spec.max !== undefined && n > spec.max)
    ) {
      return { error: "range" };
    }
    return { value: n };
  }
  if (spec.pattern) {
    try {
      if (!new RegExp(`^(?:${spec.pattern})$`, "u").test(text)) return { error: "pattern" };
    } catch {
      // A pattern JavaScript cannot read is checked by the server.
    }
  }
  if (spec.enum?.length && !spec.enum.includes(text)) return { error: "pattern" };
  return { value: text };
}

/** Install a managed provider (Settings → Apps → Install …). */
export function InstallAppPage({ providerId }: { providerId: string }) {
  const api = useApi();
  const intl = useIntl();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const manage = useMe().permissions.includes("apps.manage");
  const providers = useQuery({ ...appProvidersQuery(api), enabled: manage });
  const agent = useQuery({
    ...appAgentQuery(api),
    enabled: manage,
    // Refresh until the agent answers (installs need it).
    refetchInterval: (q) => (q.state.data?.connected ? false : 5000),
  });
  const provider = providers.data?.items.find((p) => p.id === providerId);
  const install = provider?.install;
  const [values, setValues] = useState<Record<string, string | boolean> | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  // The form starts from the provider's defaults once they are known.
  useEffect(() => {
    if (!provider || !install || values) return;
    const v: Record<string, string | boolean> = {
      displayName: provider.name,
      key: install.key,
      host: install.host,
    };
    for (const s of provider.settings) {
      const d = install.settings[s.name] ?? s.default;
      v[`settings.${s.name}`] = s.type === "bool" ? d === true : d == null ? "" : String(d);
    }
    setValues(v);
  }, [provider, install, values]);

  const submit = useMutation({
    mutationFn: (body: Schema<"InstallAppInstanceRequest">) =>
      call(api.client.POST("/api/v1/apps/instances", { body })),
    onSuccess: async (inst) => {
      await queryClient.invalidateQueries({ queryKey: appsKey });
      await navigate({ to: "/settings/apps/$instanceId", params: { instanceId: inst.id } });
    },
    onError: (err) => {
      // invalid_input names the field ("host: must be ..."): mark it and
      // move focus there.
      if (err instanceof ApiError && err.status === 400 && err.problem.detail) {
        const [field = "", reason = ""] = err.problem.detail.split(/:\s*/, 2);
        const name = field === "display_name" ? "displayName" : field;
        if (values && name in values) {
          const message =
            name === "host" && reason.includes("workspace")
              ? intl.formatMessage(messages.hostIsWorkspace, {
                  workspace: window.location.hostname,
                })
              : intl.formatMessage(messages.serverRefused);
          setErrors({ [name]: message });
          window.setTimeout(() => document.getElementById(`install-${name}`)?.focus());
        }
      }
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
      <Page title={intl.formatMessage(messages.installApps)} actions={back}>
        <Alert>
          <FormattedMessage {...messages.noPermission} />
        </Alert>
      </Page>
    );
  }
  if (providers.isPending || agent.isPending) {
    return (
      <Page title={intl.formatMessage(messages.installApps)} actions={back}>
        <div role="status" aria-label={intl.formatMessage(messages.loading)} className="p-6">
          <Spinner />
        </div>
      </Page>
    );
  }
  if (providers.isError || agent.isError) {
    return (
      <Page title={intl.formatMessage(messages.installApps)} actions={back}>
        <Alert>{errorMessage(intl, providers.error ?? agent.error)}</Alert>
      </Page>
    );
  }
  if (!provider || !install) {
    return (
      <Page title={intl.formatMessage(messages.installApps)} actions={back}>
        <Alert>
          <FormattedMessage {...messages.notInstallable} />
        </Alert>
      </Page>
    );
  }
  const title = intl.formatMessage(messages.installTitle, { name: provider.name });
  const a = agent.data;
  const free = Math.max(0, a.memTotal - a.memoryReserved);
  const fits = !a.connected || (free >= install.memoryMin && a.diskFree >= install.diskMin);
  const set = (name: string, v: string | boolean) => {
    setValues((prev) => ({ ...(prev ?? {}), [name]: v }));
    setErrors((prev) => {
      const { [name]: _, ...rest } = prev;
      return rest;
    });
  };
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (!values) return;
    const found: Record<string, string> = {};
    const key = String(values.key ?? "").trim();
    const host = String(values.host ?? "")
      .trim()
      .toLowerCase();
    if (!keyPattern.test(key)) found.key = intl.formatMessage(messages.keyInvalid);
    if (!hostPattern.test(host)) {
      found.host = intl.formatMessage(messages.hostInvalid);
    } else if (host === window.location.hostname) {
      found.host = intl.formatMessage(messages.hostIsWorkspace, { workspace: host });
    }
    const settings: Record<string, unknown> = {};
    for (const s of provider.settings) {
      const name = `settings.${s.name}`;
      const r = settingValue(s, values[name] ?? "");
      if ("error" in r) {
        found[name] =
          r.error === "required"
            ? intl.formatMessage(messages.required)
            : r.error === "range"
              ? intl.formatMessage(messages.numberRange, { min: s.min ?? 0, max: s.max ?? 0 })
              : intl.formatMessage(messages.patternInvalid);
      } else if (r.value !== undefined) {
        settings[s.name] = r.value;
      }
    }
    setErrors(found);
    const first = Object.keys(found)[0];
    if (first) {
      document.getElementById(`install-${first}`)?.focus();
      return;
    }
    const displayName = String(values.displayName ?? "").trim();
    submit.mutate({
      provider: provider.id,
      key,
      host,
      settings,
      ...(displayName ? { displayName } : {}),
    });
  };
  const refusal =
    submit.error instanceof ApiError && submit.error.status === 409
      ? explainCode(intl, submit.error.problem.code)
      : undefined;
  return (
    <Page
      title={title}
      description={intl.formatMessage(messages.installDescription)}
      actions={back}
    >
      <div className="space-y-5">
        {!a.connected && <AgentProblem agent={a} />}
        <Card>
          <CardHeader>
            <CardTitle>
              <FormattedMessage {...messages.resources} />
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <p>
              {intl.formatMessage(messages.needs, {
                min: formatBytes(intl, install.memoryMin),
                recommended: formatBytes(intl, install.memoryRecommended),
                disk: formatBytes(intl, install.diskMin),
              })}
            </p>
            {a.connected && (
              <p className="text-muted-foreground">
                {intl.formatMessage(messages.server, {
                  free: formatBytes(intl, free),
                  total: formatBytes(intl, a.memTotal),
                  disk: formatBytes(intl, a.diskFree),
                })}
              </p>
            )}
            {a.connected && !fits && (
              <Alert>
                <FormattedMessage {...messages.tooLittle} />
              </Alert>
            )}
            {a.connected && fits && free < install.memoryRecommended && (
              <Alert variant="info">
                <FormattedMessage {...messages.belowRecommended} />
              </Alert>
            )}
          </CardContent>
        </Card>
        {values && (
          <Card>
            <CardContent className="pt-6">
              <form noValidate onSubmit={onSubmit} className="space-y-5" aria-label={title}>
                <Field
                  id="install-displayName"
                  label={intl.formatMessage(messages.displayName)}
                  hint={intl.formatMessage(messages.displayNameHint)}
                  value={String(values.displayName ?? "")}
                  maxLength={100}
                  onChange={(e) => set("displayName", e.target.value)}
                  error={errors.displayName}
                />
                <Field
                  id="install-host"
                  label={intl.formatMessage(messages.host)}
                  hint={intl.formatMessage(messages.hostHint)}
                  value={String(values.host ?? "")}
                  autoCapitalize="none"
                  autoCorrect="off"
                  spellCheck={false}
                  inputMode="url"
                  onChange={(e) => set("host", e.target.value)}
                  error={errors.host}
                />
                <Field
                  id="install-key"
                  label={intl.formatMessage(messages.key)}
                  hint={intl.formatMessage(messages.keyHint)}
                  value={String(values.key ?? "")}
                  autoCapitalize="none"
                  spellCheck={false}
                  maxLength={24}
                  onChange={(e) => set("key", e.target.value)}
                  error={errors.key}
                />
                {provider.settings.map((s) => (
                  <SettingField
                    key={s.name}
                    spec={s}
                    value={values[`settings.${s.name}`] ?? ""}
                    error={errors[`settings.${s.name}`]}
                    onChange={(v) => set(`settings.${s.name}`, v)}
                  />
                ))}
                {submit.isError && Object.keys(errors).length === 0 && (
                  <Alert>{refusal ?? errorMessage(intl, submit.error)}</Alert>
                )}
                <Button type="submit" loading={submit.isPending} disabled={!a.connected || !fits}>
                  <FormattedMessage {...messages.install} />
                </Button>
              </form>
            </CardContent>
          </Card>
        )}
      </div>
    </Page>
  );
}

function SettingField({
  spec,
  value,
  error,
  onChange,
}: {
  spec: SettingSpec;
  value: string | boolean;
  error: string | undefined;
  onChange: (v: string | boolean) => void;
}) {
  const intl = useIntl();
  const id = `install-settings.${spec.name}`;
  const m = settingMessages[spec.name];
  const text = m ? intl.formatMessage(m.label) : spec.name;
  const hint = m?.hint ? intl.formatMessage(m.hint) : undefined;
  if (spec.type === "bool") {
    return (
      <div className="flex items-center gap-2">
        <input
          id={id}
          type="checkbox"
          className="size-4 accent-primary"
          checked={value === true}
          onChange={(e) => onChange(e.target.checked)}
        />
        <Label htmlFor={id}>{text}</Label>
      </div>
    );
  }
  return (
    <Field
      id={id}
      label={text}
      hint={hint}
      value={String(value)}
      inputMode={spec.type === "int" ? "numeric" : undefined}
      required={spec.required}
      onChange={(e) => onChange(e.target.value)}
      error={error}
    />
  );
}

/**
 * Remove, delete permanently (purge) or reinstall a managed instance. Each
 * destructive action asks first, says what it does, and a purge needs the
 * instance key typed. Focus moves into the confirmation and back.
 */
export function InstanceLifecycle({
  instance,
  onChange,
}: {
  instance: AppInstance;
  onChange: () => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<"remove" | "purge" | null>(null);
  const [typed, setTyped] = useState("");
  const panelId = useId();
  const removeButton = useRef<HTMLButtonElement>(null);
  const purgeButton = useRef<HTMLButtonElement>(null);
  const firstInPanel = useRef<HTMLButtonElement & HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (mode) firstInPanel.current?.focus();
  }, [mode]);
  const done = async () => {
    setMode(null);
    setTyped("");
    onChange();
    await queryClient.invalidateQueries({ queryKey: appsKey });
    heading.current?.focus();
  };
  const remove = useMutation({
    mutationFn: (purge: boolean) =>
      call(
        api.client.DELETE("/api/v1/apps/instances/{instanceId}", {
          params: {
            path: { instanceId: instance.id },
            query: purge ? { purge: true, confirm: typed } : {},
          },
        }),
      ),
    onSuccess: done,
  });
  const reinstall = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/apps/instances/{instanceId}/reinstall", {
          params: { path: { instanceId: instance.id } },
        }),
      ),
    onSuccess: done,
  });
  const cancel = () => {
    const back = mode === "purge" ? purgeButton : removeButton;
    setMode(null);
    setTyped("");
    remove.reset();
    window.setTimeout(() => back.current?.focus());
  };
  const state = instance.state;
  // After a failed purge only another purge may follow (purge_failed).
  const purgeFailed = state === "failed" && instance.healthCode === "purge_failed";
  const removable =
    (state === "active" || state === "degraded" || state === "failed") && !purgeFailed;
  const retained = state === "removed" && instance.dataRetained;
  const failure = remove.error ?? reinstall.error;
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <span ref={heading} tabIndex={-1} className="outline-none">
            <FormattedMessage {...messages.lifecycle} />
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        {state === "removing" && (
          <p role="status">
            <FormattedMessage {...messages.removing} />
          </p>
        )}
        {state === "removed" && !instance.dataRetained && (
          <p>
            <FormattedMessage {...messages.purged} />
          </p>
        )}
        {retained && (
          <div className="space-y-2">
            <p>
              <FormattedMessage {...messages.reinstallExplain} />
            </p>
            <Button loading={reinstall.isPending} onClick={() => reinstall.mutate()}>
              <FormattedMessage {...messages.reinstall} />
            </Button>
          </div>
        )}
        {failure && (
          <Alert>
            {(failure instanceof ApiError && explainCode(intl, failure.problem.code)) ||
              errorMessage(intl, failure)}
          </Alert>
        )}
        {purgeFailed && (
          <p>
            <FormattedMessage {...messages.purgeFailed} />
          </p>
        )}
        {(removable || retained || purgeFailed) && mode === null && (
          <div className="flex flex-wrap gap-2">
            {removable && (
              <Button ref={removeButton} variant="outline" onClick={() => setMode("remove")}>
                <FormattedMessage {...messages.remove} />
              </Button>
            )}
            <Button ref={purgeButton} variant="destructive-ghost" onClick={() => setMode("purge")}>
              <FormattedMessage {...messages.purge} />
            </Button>
          </div>
        )}
        {mode && (
          // Escape cancels the confirmation from any control in it.
          <fieldset
            id={panelId}
            className="space-y-3 rounded-lg border border-destructive/40 p-4"
            aria-describedby={`${panelId}-explain`}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                e.preventDefault();
                cancel();
              }
            }}
          >
            <legend className="px-1 font-medium">
              <FormattedMessage {...(mode === "remove" ? messages.remove : messages.purge)} />
            </legend>
            <p id={`${panelId}-explain`}>
              <FormattedMessage
                {...(mode === "remove" ? messages.removeExplain : messages.purgeExplain)}
              />
            </p>
            {mode === "purge" && (
              <div className="space-y-2">
                <Label htmlFor={`${panelId}-key`}>
                  {intl.formatMessage(messages.purgeType, { key: instance.key })}
                </Label>
                <Input
                  ref={firstInPanel}
                  id={`${panelId}-key`}
                  value={typed}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  onChange={(e) => setTyped(e.target.value)}
                />
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              {mode === "remove" && (
                <Button ref={firstInPanel} variant="outline" onClick={cancel}>
                  <FormattedMessage {...messages.cancel} />
                </Button>
              )}
              <Button
                variant="destructive"
                loading={remove.isPending}
                disabled={mode === "purge" && typed !== instance.key}
                onClick={() => remove.mutate(mode === "purge")}
              >
                <FormattedMessage
                  {...(mode === "remove" ? messages.confirmRemove : messages.confirmPurge)}
                />
              </Button>
              {mode === "purge" && (
                <Button variant="outline" onClick={cancel}>
                  <FormattedMessage {...messages.cancel} />
                </Button>
              )}
            </div>
          </fieldset>
        )}
      </CardContent>
    </Card>
  );
}
