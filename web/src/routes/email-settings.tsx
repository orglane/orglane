import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";
import { invitationsKey } from "./invitations";

const settingsKey = ["email-settings"] as const;
const messages = defineMessages({
  title: { id: "emailSettings.title", defaultMessage: "System email" },
  description: {
    id: "emailSettings.description",
    defaultMessage: "Send invitations and notifications from your workspace.",
  },
  host: { id: "emailSettings.host", defaultMessage: "SMTP host" },
  port: { id: "emailSettings.port", defaultMessage: "SMTP port" },
  tls: { id: "emailSettings.tls", defaultMessage: "Connection security" },
  starttls: { id: "emailSettings.starttls", defaultMessage: "STARTTLS (usually port 587)" },
  implicit: { id: "emailSettings.implicit", defaultMessage: "TLS (usually port 465)" },
  none: { id: "emailSettings.none", defaultMessage: "No TLS (trusted local relay only)" },
  username: { id: "emailSettings.username", defaultMessage: "SMTP username" },
  password: { id: "emailSettings.password", defaultMessage: "SMTP password" },
  passwordHint: {
    id: "emailSettings.passwordHint",
    defaultMessage:
      "Leave blank to keep the saved password. Remove the username to disable authentication.",
  },
  from: { id: "emailSettings.from", defaultMessage: "From address" },
  fromHint: {
    id: "emailSettings.fromHint",
    defaultMessage: "A sender address such as no-reply@company.com, optionally with a sender name.",
  },
  save: { id: "emailSettings.save", defaultMessage: "Save email settings" },
  saved: { id: "emailSettings.saved", defaultMessage: "Email settings saved." },
  removed: {
    id: "emailSettings.removed",
    defaultMessage: "Email settings removed. Invitations will produce links to share.",
  },
  remove: { id: "emailSettings.remove", defaultMessage: "Remove email settings" },
  config: {
    id: "emailSettings.config",
    defaultMessage:
      "These settings are managed by the server configuration. You can send a test email here.",
  },
  unconfigured: {
    id: "emailSettings.unconfigured",
    defaultMessage: "Email is not configured. Invitations produce links you can share yourself.",
  },
  test: { id: "emailSettings.test", defaultMessage: "Send test email" },
  to: { id: "emailSettings.to", defaultMessage: "Test recipient" },
  tested: {
    id: "emailSettings.tested",
    defaultMessage: "Test email delivered to the SMTP server. Check the recipient's inbox.",
  },
  testHint: {
    id: "emailSettings.testHint",
    defaultMessage: "Save your settings before sending a test.",
  },
  invalid: {
    id: "emailSettings.invalid",
    defaultMessage: "Check the SMTP host, port, sender and credentials, then try again.",
  },
  conflict: {
    id: "emailSettings.conflict",
    defaultMessage:
      "Save email settings first. Settings managed by server configuration cannot be changed here.",
  },
  sendFailed: {
    id: "emailSettings.sendFailed",
    defaultMessage:
      "The SMTP server could not deliver the test email. Check the host, port, TLS mode and credentials.",
  },
  forbidden: {
    id: "emailSettings.forbidden",
    defaultMessage: "You don't have permission to manage system email.",
  },
  retry: { id: "common.retry", defaultMessage: "Try again" },
});

export function EmailSettingsPage() {
  const intl = useIntl();
  const me = useMe();
  return (
    <Page
      title={intl.formatMessage(messages.title)}
      description={intl.formatMessage(messages.description)}
    >
      {me.permissions.includes("instance.manage") ? (
        <EmailSettings />
      ) : (
        <Alert>
          <FormattedMessage {...messages.forbidden} />
        </Alert>
      )}
    </Page>
  );
}

function EmailSettings() {
  const intl = useIntl();
  const api = useApi();
  const settings = useQuery({
    queryKey: settingsKey,
    queryFn: () => call(api.client.GET("/api/v1/settings/email")),
  });
  return (
    <div className="space-y-6">
      {settings.isError && (
        <>
          <Alert>{errorMessage(intl, settings.error)}</Alert>
          <Button variant="outline" onClick={() => settings.refetch()}>
            <FormattedMessage {...messages.retry} />
          </Button>
        </>
      )}
      {settings.data && (
        <SettingsForm key={JSON.stringify(settings.data)} settings={settings.data} />
      )}
    </div>
  );
}

function SettingsForm({ settings }: { settings: Schema<"EmailSettings"> }) {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const readonly = settings.source === "config";
  const [values, setValues] = useState({
    host: settings.host ?? "",
    port: settings.port ?? 587,
    tls: settings.tls ?? "starttls",
    username: settings.username ?? "",
    password: "",
    from: settings.from ?? "",
  });
  const [to, setTo] = useState("");
  const [notice, setNotice] = useState<"saved" | "removed" | "tested">();
  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ["invitation-options"] });
    await queryClient.invalidateQueries({ queryKey: invitationsKey });
  };
  const save = useMutation({
    mutationFn: () =>
      call(
        api.client.PUT("/api/v1/settings/email", {
          body: {
            host: values.host,
            port: values.port,
            tls: values.tls,
            username: values.username,
            from: values.from,
            ...(values.password === "" ? {} : { password: values.password }),
          },
        }),
      ),
    onSuccess: async () => {
      setNotice("saved");
      await refresh();
    },
  });
  const remove = useMutation({
    mutationFn: () => call(api.client.DELETE("/api/v1/settings/email")),
    onSuccess: async () => {
      setNotice("removed");
      await refresh();
      await queryClient.invalidateQueries({ queryKey: settingsKey });
    },
  });
  const test = useMutation({
    mutationFn: () => call(api.client.POST("/api/v1/settings/email/test", { body: { to } })),
    onSuccess: () => setNotice("tested"),
  });
  const error = save.error ?? remove.error ?? test.error;
  const busy = save.isPending || remove.isPending || test.isPending;
  return (
    <div className="space-y-6">
      {settings.source === "none" && !save.isSuccess && (
        <p className="text-muted-foreground text-sm">
          <FormattedMessage {...messages.unconfigured} />
        </p>
      )}
      {readonly && (
        <p className="text-muted-foreground text-sm">
          <FormattedMessage {...messages.config} />
        </p>
      )}
      <Card className="p-5 sm:p-6">
        <form
          className="grid gap-5 sm:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault();
            setNotice(undefined);
            save.mutate();
          }}
        >
          <Field
            label={intl.formatMessage(messages.host)}
            required
            disabled={readonly}
            value={values.host}
            onChange={(e) => setValues({ ...values, host: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.port)}
            type="number"
            min={1}
            max={65535}
            required
            disabled={readonly}
            value={values.port}
            onChange={(e) => setValues({ ...values, port: Number(e.target.value) })}
          />
          <label className="flex flex-col gap-2 font-medium text-sm">
            <FormattedMessage {...messages.tls} />
            <select
              className="h-10 rounded-md border bg-surface px-3"
              disabled={readonly}
              value={values.tls}
              onChange={(e) => {
                const tls = e.target.value as Schema<"EmailTLSMode">;
                setValues({
                  ...values,
                  tls,
                  port: tls === "tls" ? 465 : tls === "none" ? 25 : 587,
                });
              }}
            >
              <option value="starttls">{intl.formatMessage(messages.starttls)}</option>
              <option value="tls">{intl.formatMessage(messages.implicit)}</option>
              <option value="none">{intl.formatMessage(messages.none)}</option>
            </select>
          </label>
          <Field
            label={intl.formatMessage(messages.from)}
            hint={intl.formatMessage(messages.fromHint)}
            required
            disabled={readonly}
            value={values.from}
            onChange={(e) => setValues({ ...values, from: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.username)}
            autoComplete="off"
            disabled={readonly}
            value={values.username}
            onChange={(e) => setValues({ ...values, username: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.password)}
            hint={intl.formatMessage(messages.passwordHint)}
            type="password"
            autoComplete="new-password"
            disabled={readonly}
            value={values.password}
            onChange={(e) => setValues({ ...values, password: e.target.value })}
          />
          {!readonly && (
            <div className="flex flex-wrap gap-2 sm:col-span-2">
              <Button type="submit" loading={save.isPending} disabled={busy}>
                <FormattedMessage {...messages.save} />
              </Button>
              {(settings.source === "settings" || save.isSuccess) && (
                <Button
                  variant="destructive-ghost"
                  type="button"
                  loading={remove.isPending}
                  disabled={busy}
                  onClick={() => {
                    setNotice(undefined);
                    remove.mutate();
                  }}
                >
                  <FormattedMessage {...messages.remove} />
                </Button>
              )}
            </div>
          )}
        </form>
      </Card>
      {notice && (
        <p role="status">
          <FormattedMessage {...messages[notice]} />
        </p>
      )}
      {error && (
        <Alert>
          {errorMessage(intl, error, {
            400: messages.invalid,
            409: messages.conflict,
            502: messages.sendFailed,
          })}
        </Alert>
      )}
      <Card className="p-5 sm:p-6">
        <form
          className="max-w-md space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            setNotice(undefined);
            test.mutate();
          }}
        >
          <Field
            label={intl.formatMessage(messages.to)}
            hint={intl.formatMessage(messages.testHint)}
            type="email"
            required
            value={to}
            onChange={(e) => setTo(e.target.value)}
          />
          <Button
            variant="outline"
            type="submit"
            loading={test.isPending}
            disabled={busy || (settings.source === "none" && !save.isSuccess)}
          >
            <FormattedMessage {...messages.test} />
          </Button>
        </form>
      </Card>
    </div>
  );
}
