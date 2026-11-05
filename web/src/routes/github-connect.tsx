import { useMutation } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError, call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";

const m = defineMessages({
  title: { id: "github.connect.title", defaultMessage: "Connect GitHub" },
  description: {
    id: "github.connect.description",
    defaultMessage:
      "Connect an organization you own using a GitHub App. You will create the App on GitHub, then install it on your organization.",
  },
  organization: { id: "github.connect.organization", defaultMessage: "GitHub organization" },
  key: { id: "github.connect.key", defaultMessage: "App key in Orglane" },
  keyHelp: {
    id: "github.connect.keyHelp",
    defaultMessage: "A unique name for this connection, such as github or github-design.",
  },
  permissions: {
    id: "github.connect.permissions",
    defaultMessage:
      "The App requests Members read and write, plus Metadata, Pull requests and Issues read access. Choose which repositories it can access when you install it. Account linking and access management will be available in a later update.",
  },
  begin: { id: "github.connect.begin", defaultMessage: "Prepare GitHub App" },
  continue: { id: "github.connect.continue", defaultMessage: "Continue to GitHub" },
  create: { id: "github.connect.create", defaultMessage: "Create your GitHub App" },
  install: { id: "github.connect.install", defaultMessage: "Install your GitHub App" },
  ready: {
    id: "github.connect.ready",
    defaultMessage:
      "Continue on GitHub in this browser, then return here automatically. This step expires after one hour.",
  },
  done: { id: "github.connect.done", defaultMessage: "GitHub connected" },
  doneDescription: {
    id: "github.connect.doneDescription",
    defaultMessage: "The App installation and organization have been verified.",
  },
  waiting: { id: "github.connect.waiting", defaultMessage: "Verifying your GitHub connection…" },
  back: { id: "github.connect.back", defaultMessage: "Back to Apps" },
  detail: { id: "github.connect.detail", defaultMessage: "View connection" },
  failed: {
    id: "github.connect.failed",
    defaultMessage:
      "This connection could not be completed. The callback may have expired or already been used. Return to Apps and resume the connecting instance; if the App was not saved, prepare it again or use manual setup.",
  },
  denied: {
    id: "github.connect.denied",
    defaultMessage: "You need permission to manage apps to connect GitHub.",
  },
  manual: { id: "github.connect.manual", defaultMessage: "Use manual setup" },
  manualHelp: {
    id: "github.connect.manualHelp",
    defaultMessage:
      "Create a GitHub App with the permissions above. Set its homepage to this workspace, webhook URL to the address shown above, setup URL to this workspace followed by /settings/apps/github/callback, and user authorization callback URL to this workspace followed by /me/accounts/github/callback. Keep Request user authorization during installation off. Enter its credentials below; they are stored encrypted and never shown again.",
  },
  appId: { id: "github.connect.appId", defaultMessage: "App ID" },
  clientId: { id: "github.connect.clientId", defaultMessage: "Client ID" },
  privateKey: { id: "github.connect.privateKey", defaultMessage: "Private key (PEM)" },
  clientSecret: { id: "github.connect.clientSecret", defaultMessage: "Client secret" },
  webhookSecret: {
    id: "github.connect.webhookSecret",
    defaultMessage: "Webhook secret (at least 32 characters)",
  },
  keyReserved: {
    id: "github.connect.keyReserved",
    defaultMessage:
      "This app key is already reserved. Choose a different app key, such as github-design, or resume the existing connection from Apps.",
  },
  invalidPrivateKey: {
    id: "github.connect.invalidPrivateKey",
    defaultMessage:
      "Generate a private key in this GitHub App’s settings and paste the complete PEM file, including its BEGIN and END lines. Your other entries are kept so you can try again.",
  },
  resume: { id: "github.connect.resume", defaultMessage: "Resume GitHub connection" },
});

export function GitHubBrowserStep({ step }: { step: Schema<"GitHubConnectStep"> }) {
  const intl = useIntl();
  const u = step.url ? new URL(step.url) : null;
  if (
    u?.origin !== "https://github.com" ||
    u.username ||
    u.password ||
    !["GET", "POST"].includes(step.method ?? "")
  )
    return (
      <Alert>
        <FormattedMessage {...m.failed} />
      </Alert>
    );
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...(step.name === "create_app" ? m.create : m.install)} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...m.ready} />
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-wrap gap-3">
        {step.method === "POST" ? (
          <form action={u.href} method="POST">
            {Object.entries(step.form ?? {}).map(([name, value]) => (
              <input key={name} type="hidden" name={name} value={value} />
            ))}
            <Button type="submit">{intl.formatMessage(m.continue)}</Button>
          </form>
        ) : (
          <Button asChild>
            <a href={u.href}>
              <FormattedMessage {...m.continue} />
            </a>
          </Button>
        )}
        <Button asChild variant="outline">
          <Link to="/settings/apps/$instanceId" params={{ instanceId: step.instanceId }}>
            <FormattedMessage {...m.detail} />
          </Link>
        </Button>
      </CardContent>
    </Card>
  );
}

export function GitHubConnectForm({ instance }: { instance?: Schema<"AppInstance"> }) {
  const api = useApi();
  const intl = useIntl();
  const [organization, setOrganization] = useState(String(instance?.settings.organization ?? ""));
  const [key, setKey] = useState(instance?.key ?? "github");
  const [manual, setManual] = useState(false);
  const begin = useMutation({
    mutationFn: async (data: FormData) => {
      const credentials = manual
        ? {
            appId: String(data.get("appId")),
            clientId: String(data.get("clientId")),
            privateKey: String(data.get("privateKey")),
            clientSecret: String(data.get("clientSecret")),
            webhookSecret: String(data.get("webhookSecret")),
          }
        : undefined;
      return call(
        api.client.POST("/api/v1/apps/github/connect", {
          body: {
            organization,
            key,
            ...(instance ? { instanceId: instance.id } : {}),
            ...(credentials ? { manual: credentials } : {}),
          },
        }),
      );
    },
  });
  const errorCode = begin.error instanceof ApiError ? begin.error.problem.code : undefined;
  const keyReserved = begin.isError && errorCode === "key_reserved";
  const invalidPrivateKey = begin.isError && errorCode === "invalid_private_key";
  if (begin.data) return <GitHubBrowserStep step={begin.data} />;
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...(instance ? m.resume : m.title)} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...m.description} />
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="max-w-xl space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const data = new FormData(e.currentTarget);
            begin.mutate(data);
          }}
        >
          <p className="text-sm text-muted-foreground">
            <FormattedMessage {...m.permissions} />
          </p>
          {begin.isError && !keyReserved && !invalidPrivateKey && (
            <Alert>{errorMessage(intl, begin.error)}</Alert>
          )}
          <div className="space-y-2">
            <Label htmlFor="github-organization">
              <FormattedMessage {...m.organization} />
            </Label>
            <Input
              id="github-organization"
              autoComplete="off"
              required
              maxLength={39}
              pattern={"[a-zA-Z0-9][a-zA-Z0-9\\-]{0,38}"}
              value={organization}
              onChange={(e) => setOrganization(e.target.value.trim())}
              disabled={!!instance || begin.isPending}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="github-key">
              <FormattedMessage {...m.key} />
            </Label>
            <Input
              id="github-key"
              aria-describedby={
                keyReserved ? "github-key-help github-key-error" : "github-key-help"
              }
              aria-invalid={keyReserved || undefined}
              required
              minLength={2}
              maxLength={24}
              pattern={"[a-z][a-z0-9\\-]{0,22}[a-z0-9]"}
              value={key}
              onChange={(e) => {
                setKey(e.target.value);
                if (keyReserved) begin.reset();
              }}
              disabled={!!instance || begin.isPending}
            />
            <p id="github-key-help" className="text-xs text-muted-foreground">
              <FormattedMessage {...m.keyHelp} />
            </p>
            {keyReserved && (
              <Alert id="github-key-error">
                <FormattedMessage {...m.keyReserved} />
              </Alert>
            )}
          </div>
          {instance && !instance.settings.app_id && (
            <Button
              type="button"
              variant="link"
              onClick={() => setManual(!manual)}
              disabled={begin.isPending}
            >
              <FormattedMessage {...m.manual} />
            </Button>
          )}
          {manual && (
            <fieldset className="space-y-4" disabled={begin.isPending}>
              <p className="text-sm text-muted-foreground">
                <FormattedMessage {...m.manualHelp} />
              </p>
              {(["appId", "clientId", "clientSecret", "webhookSecret"] as const).map((field) => (
                <div key={field} className="space-y-2">
                  <Label htmlFor={`github-${field}`}>
                    <FormattedMessage {...m[field]} />
                  </Label>
                  <Input
                    id={`github-${field}`}
                    name={field}
                    autoComplete="off"
                    type={field.endsWith("Secret") ? "password" : "text"}
                    required
                    minLength={field === "webhookSecret" ? 32 : 1}
                    maxLength={1000}
                  />
                </div>
              ))}
              <div className="space-y-2">
                <Label htmlFor="github-privateKey">
                  <FormattedMessage {...m.privateKey} />
                </Label>
                <textarea
                  id="github-privateKey"
                  name="privateKey"
                  aria-invalid={invalidPrivateKey || undefined}
                  aria-describedby={invalidPrivateKey ? "github-privateKey-error" : undefined}
                  onChange={() => {
                    if (invalidPrivateKey) begin.reset();
                  }}
                  autoComplete="off"
                  required
                  maxLength={16384}
                  rows={5}
                  className="w-full rounded-md border bg-surface p-3 font-mono text-xs"
                />
                {invalidPrivateKey && (
                  <Alert id="github-privateKey-error">
                    <FormattedMessage {...m.invalidPrivateKey} />
                  </Alert>
                )}
              </div>
            </fieldset>
          )}
          <Button type="submit" loading={begin.isPending}>
            <FormattedMessage {...(instance ? m.resume : m.begin)} />
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

export function GitHubConnectPage() {
  const intl = useIntl();
  const manage = useMe().permissions.includes("apps.manage");
  return (
    <Page
      title={intl.formatMessage(m.title)}
      actions={
        <Button asChild variant="ghost">
          <Link to="/settings/apps">
            <FormattedMessage {...m.back} />
          </Link>
        </Button>
      }
    >
      {manage ? (
        <GitHubConnectForm />
      ) : (
        <Alert>
          <FormattedMessage {...m.denied} />
        </Alert>
      )}
    </Page>
  );
}

export function GitHubConnectCallbackPage() {
  const api = useApi();
  const intl = useIntl();
  const manage = useMe().permissions.includes("apps.manage");
  const started = useRef(false);
  const [params] = useState(() => new URLSearchParams(window.location.search));
  const invalidCallback =
    ["state", "code", "installation_id"].some((key) => params.getAll(key).length > 1) ||
    !params.get("state") ||
    params.has("error") ||
    params.has("code") === params.has("installation_id");
  const complete = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/apps/github/connect/complete", {
          body: {
            state: params.get("state") ?? "",
            ...(params.has("code") ? { code: params.get("code") ?? "" } : {}),
            ...(params.has("installation_id")
              ? { installationId: params.get("installation_id") ?? "" }
              : {}),
          },
        }),
      ),
  });
  const { mutate } = complete;
  useEffect(() => {
    // Remove temporary codes and state from browser history before calling the API.
    window.history.replaceState(window.history.state, "", window.location.pathname);
    if (!manage || started.current) return;
    started.current = true;
    if (invalidCallback) return;
    mutate();
  }, [manage, mutate, invalidCallback]);
  return (
    <Page title={intl.formatMessage(complete.data?.done ? m.done : m.title)}>
      {!manage ? (
        <Alert>
          <FormattedMessage {...m.denied} />
        </Alert>
      ) : complete.data ? (
        complete.data.done ? (
          <Card>
            <CardHeader>
              <CardTitle>
                <FormattedMessage {...m.done} />
              </CardTitle>
              <CardDescription>
                <FormattedMessage {...m.doneDescription} />
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Button asChild>
                <Link
                  to="/settings/apps/$instanceId"
                  params={{ instanceId: complete.data.instanceId }}
                >
                  <FormattedMessage {...m.detail} />
                </Link>
              </Button>
            </CardContent>
          </Card>
        ) : (
          <GitHubBrowserStep step={complete.data} />
        )
      ) : !invalidCallback && !complete.isError ? (
        <p role="status">
          <FormattedMessage {...m.waiting} />
        </p>
      ) : (
        <Alert>
          <FormattedMessage {...m.failed} />
        </Alert>
      )}
      <Button asChild variant="link">
        <Link to="/settings/apps">
          <FormattedMessage {...m.back} />
        </Link>
      </Button>
    </Page>
  );
}

const keyMessages = defineMessages({
  title: { id: "github.key.title", defaultMessage: "Replace App private key" },
  description: {
    id: "github.key.description",
    defaultMessage:
      "Generate a new private key in your GitHub App settings, then upload its PEM file. Orglane keeps the working key until the replacement is verified. Revoke the old key on GitHub after verification.",
  },
  file: { id: "github.key.file", defaultMessage: "New private key (.pem)" },
  upload: { id: "github.key.upload", defaultMessage: "Verify replacement key" },
  accepted: {
    id: "github.key.accepted",
    defaultMessage:
      "Replacement key accepted and queued for verification. The working key remains active.",
  },
  verifying: {
    id: "github.key.verifying",
    defaultMessage:
      "Verifying the replacement key. The result is not confirmed yet; keep the working key on GitHub while Orglane retries.",
  },
  failed: {
    id: "github.key.failed",
    defaultMessage:
      "Replacement key verification failed. The working key is still active. Generate a new PEM key for this GitHub App, check the App ID and installation, then upload it again. Keep the working key on GitHub.",
  },
  verified: {
    id: "github.key.verified",
    defaultMessage:
      "Replacement key verified and active. You can now revoke the old private key in your GitHub App settings.",
  },
  cancelling: {
    id: "github.key.cancelling",
    defaultMessage:
      "Restoring the working key before cancelling this replacement. Keep both keys on GitHub until this is confirmed.",
  },
  cancelled: {
    id: "github.key.cancelled",
    defaultMessage: "Replacement cancelled. The working key remains active.",
  },
  activated_by_admin: {
    id: "github.key.activatedByAdmin",
    defaultMessage:
      "Replacement key activated by an admin without verification. Orglane did not confirm that GitHub accepts it. Keep the old key on GitHub until Orglane's calls to GitHub succeed with the new one.",
  },
  discarded_by_admin: {
    id: "github.key.discardedByAdmin",
    defaultMessage:
      "Replacement key discarded by an admin without verification. The working key remains active.",
  },
  inProgress: {
    id: "github.key.inProgress",
    defaultMessage:
      "Orglane is still changing this key: a replacement is being verified or cancelled, or a call to GitHub may still be running. Wait a few minutes, reload the page and check the key's status, then try again.",
  },
  invalid: { id: "github.key.invalid", defaultMessage: "Choose a PEM file of at most 16 KiB." },
});

export function GitHubPrivateKeyUpload({
  instanceId,
  disabled,
  rotation,
  onChange,
}: {
  instanceId: string;
  disabled: boolean;
  rotation: Schema<"AppCredentialRotationStatus"> | undefined;
  onChange: () => Promise<void>;
}) {
  const api = useApi();
  const intl = useIntl();
  const [invalid, setInvalid] = useState(false);
  const upload = useMutation({
    mutationFn: async (file: File) =>
      call(
        api.client.PUT("/api/v1/apps/instances/{instanceId}/credentials/{credentialName}", {
          params: { path: { instanceId, credentialName: "app_private_key" } },
          body: { value: await file.text() },
        }),
      ),
    onSuccess: onChange,
  });
  return (
    <form
      className="space-y-3 border-t pt-4"
      onSubmit={(e) => {
        e.preventDefault();
        const form = e.currentTarget;
        const input = form.elements.namedItem("privateKey");
        const file = input instanceof HTMLInputElement ? input.files?.[0] : undefined;
        if (!(file instanceof File) || file.size === 0 || file.size > 16384) {
          setInvalid(true);
          return;
        }
        setInvalid(false);
        upload.mutate(file, { onSuccess: () => form.reset() });
      }}
    >
      <h3 className="font-medium text-sm">
        <FormattedMessage {...keyMessages.title} />
      </h3>
      <p className="max-w-xl text-muted-foreground text-sm">
        <FormattedMessage {...keyMessages.description} />
      </p>
      <Label htmlFor="github-replacement-key">
        <FormattedMessage {...keyMessages.file} />
      </Label>
      <Input
        id="github-replacement-key"
        name="privateKey"
        type="file"
        accept=".pem"
        required
        disabled={disabled || upload.isPending}
      />
      {invalid && (
        <Alert>
          <FormattedMessage {...keyMessages.invalid} />
        </Alert>
      )}
      {upload.isError && (
        <Alert>
          {upload.error instanceof ApiError &&
          upload.error.problem.code === "rotation_in_progress" ? (
            <FormattedMessage {...keyMessages.inProgress} />
          ) : (
            errorMessage(intl, upload.error)
          )}
        </Alert>
      )}
      {rotation && !upload.isPending && (
        <Alert
          variant={
            rotation.state === "failed"
              ? "error"
              : rotation.state === "verified"
                ? "success"
                : "info"
          }
        >
          <FormattedMessage {...keyMessages[rotation.state]} />
        </Alert>
      )}
      <Button type="submit" variant="outline" loading={upload.isPending} disabled={disabled}>
        <FormattedMessage {...keyMessages.upload} />
      </Button>
    </form>
  );
}
