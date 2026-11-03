import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Laptop, Plus, Smartphone, Trash2 } from "lucide-react";
import { type ReactNode, useState } from "react";
import {
  defineMessage,
  defineMessages,
  FormattedMessage,
  type MessageTag,
  useIntl,
} from "react-intl";

import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { passkeysKey, useAddPasskey } from "@/lib/passkeys";
import { useEndSession } from "@/lib/session-guard";
import { exactTime, relativeTime } from "@/lib/time";
import { isCancellation, passkeysSupported } from "@/lib/webauthn";

export const sessionsKey = ["sessions"] as const;

const messages = defineMessages({
  title: { id: "security.title", defaultMessage: "Sign-in & security" },
  description: {
    id: "security.description",
    defaultMessage: "Manage how you sign in and where you're signed in.",
  },
  passkeys: { id: "security.passkeys.title", defaultMessage: "Passkeys" },
  passkeysDescription: {
    id: "security.passkeys.description",
    defaultMessage: "Sign in with your fingerprint, face or screen lock instead of a password.",
  },
  addPasskey: { id: "security.passkeys.add", defaultMessage: "Add passkey" },
  noPasskeys: {
    id: "security.passkeys.empty",
    defaultMessage: "No passkeys yet. Add one to sign in faster and more safely.",
  },
  passkeysUnsupported: {
    id: "security.passkeys.unsupported",
    defaultMessage: "This browser doesn't support passkeys.",
  },
  passkeyMeta: defineMessage<{ created: string; used: string }>({
    id: "security.passkeys.meta",
    defaultMessage: "Added {created}{used, select, never {} other { · Last used {used}}}",
  }),
  removePasskey: defineMessage<{ name: string }>({
    id: "security.passkeys.remove",
    defaultMessage: "Remove passkey {name}",
  }),
  confirmRemove: { id: "security.passkeys.confirmRemove", defaultMessage: "Remove this passkey?" },
  remove: { id: "security.passkeys.removeConfirm", defaultMessage: "Remove" },
  cancel: { id: "common.cancel", defaultMessage: "Cancel" },
  passkeyFailed: {
    id: "security.passkeys.failed",
    defaultMessage: "The passkey couldn't be created. Try again.",
  },
  sessions: { id: "security.sessions.title", defaultMessage: "Where you're signed in" },
  sessionsDescription: {
    id: "security.sessions.description",
    defaultMessage: "Sign out of any session you don't recognize. It ends right away.",
  },
  thisDevice: { id: "security.sessions.current", defaultMessage: "This device" },
  sessionSignedIn: defineMessage<{ created: string; method: string; time: MessageTag }>({
    id: "security.sessions.signedIn",
    defaultMessage:
      "Signed in <time>{created}</time> with {method, select, passkey {a passkey} other {a password}}",
  }),
  sessionActive: defineMessage<{ lastSeen: string; time: MessageTag }>({
    id: "security.sessions.lastActive",
    defaultMessage: "Last active <time>{lastSeen}</time>",
  }),
  signOut: { id: "security.sessions.signOut", defaultMessage: "Sign out" },
  signOutSession: defineMessage<{ device: string; created: string }>({
    id: "security.sessions.signOutLabel",
    defaultMessage: "Sign out of {device}, signed in {created}",
  }),
  loadFailed: { id: "security.loadFailed", defaultMessage: "This list couldn't be loaded." },
  retry: { id: "common.retry", defaultMessage: "Try again" },
});

function Section({
  title,
  description,
  action,
  children,
}: {
  title: ReactNode;
  description: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card className="overflow-hidden">
      <div className="flex flex-col items-start gap-3 border-b p-5 sm:flex-row sm:justify-between sm:px-6">
        <div className="min-w-0 sm:flex-1">
          <CardTitle>{title}</CardTitle>
          <CardDescription className="mt-1">{description}</CardDescription>
        </div>
        {action}
      </div>
      {children}
    </Card>
  );
}

function Row({
  icon,
  children,
  actions,
}: {
  icon: ReactNode;
  children: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <li className="flex flex-wrap items-center gap-x-4 gap-y-3 px-5 py-4 sm:px-6">
      <span className="flex size-10 shrink-0 items-center justify-center rounded-lg border bg-canvas text-muted-foreground">
        {icon}
      </span>
      <div className="min-w-0 flex-1">{children}</div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2 max-sm:w-full max-sm:justify-end">
          {actions}
        </div>
      )}
    </li>
  );
}

function LoadingRows() {
  return (
    <ul className="divide-y" aria-hidden="true">
      {[0, 1].map((i) => (
        <li key={i} className="flex items-center gap-4 px-5 py-4 sm:px-6">
          <Skeleton className="size-10 rounded-lg" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-4 w-40" />
            <Skeleton className="h-3 w-64 max-w-full" />
          </div>
        </li>
      ))}
    </ul>
  );
}

function LoadError({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-5 py-4 text-muted-foreground text-sm sm:px-6">
      <FormattedMessage {...messages.loadFailed} />
      <Button variant="outline" size="sm" onClick={onRetry}>
        <FormattedMessage {...messages.retry} />
      </Button>
    </div>
  );
}

function PasskeyRow({ passkey }: { passkey: Schema<"Passkey"> }) {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () =>
      call(
        api.client.DELETE("/api/v1/me/passkeys/{passkeyId}", {
          params: { path: { passkeyId: passkey.id } },
        }),
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: passkeysKey }),
  });
  return (
    <Row
      icon={<KeyRound className="size-5" aria-hidden="true" />}
      actions={
        confirming ? (
          <>
            <span className="text-sm">
              <FormattedMessage {...messages.confirmRemove} />
            </span>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setConfirming(false)}
              disabled={remove.isPending}
            >
              <FormattedMessage {...messages.cancel} />
            </Button>
            <Button
              variant="destructive"
              size="sm"
              loading={remove.isPending}
              onClick={() => remove.mutate()}
              autoFocus
            >
              <FormattedMessage {...messages.remove} />
            </Button>
          </>
        ) : (
          <Button
            variant="destructive-ghost"
            size="icon-sm"
            aria-label={intl.formatMessage(messages.removePasskey, { name: passkey.name })}
            onClick={() => setConfirming(true)}
          >
            <Trash2 />
          </Button>
        )
      }
    >
      <p className="truncate font-medium text-sm">{passkey.name}</p>
      <p className="mt-0.5 text-muted-foreground text-[13px]">
        <FormattedMessage
          {...messages.passkeyMeta}
          values={{
            created: intl.formatDate(passkey.createdAt, { dateStyle: "medium" }),
            used: passkey.lastUsedAt ? relativeTime(intl, passkey.lastUsedAt) : "never",
          }}
        />
      </p>
      {remove.isError && (
        <p className="mt-1 text-destructive-subtle-foreground text-[13px]">
          {errorMessage(intl, remove.error)}
        </p>
      )}
    </Row>
  );
}

function Passkeys() {
  const intl = useIntl();
  const api = useApi();
  const passkeys = useQuery({
    queryKey: passkeysKey,
    queryFn: () => call(api.client.GET("/api/v1/me/passkeys")),
  });
  const add = useAddPasskey();
  const supported = passkeysSupported();
  const addError =
    add.isError && !isCancellation(add.error)
      ? add.error instanceof DOMException
        ? intl.formatMessage(messages.passkeyFailed)
        : errorMessage(intl, add.error, { 400: messages.passkeyFailed })
      : undefined;

  return (
    <Section
      title={<FormattedMessage {...messages.passkeys} />}
      description={<FormattedMessage {...messages.passkeysDescription} />}
      action={
        supported && (
          <Button variant="outline" size="sm" loading={add.isPending} onClick={() => add.mutate()}>
            <Plus />
            <FormattedMessage {...messages.addPasskey} />
          </Button>
        )
      }
    >
      {addError && <Alert className="mx-5 mt-4 sm:mx-6">{addError}</Alert>}
      {passkeys.isPending ? (
        <LoadingRows />
      ) : passkeys.isError ? (
        <LoadError onRetry={() => passkeys.refetch()} />
      ) : passkeys.data.items.length === 0 ? (
        <p className="px-5 py-6 text-muted-foreground text-sm sm:px-6">
          <FormattedMessage {...(supported ? messages.noPasskeys : messages.passkeysUnsupported)} />
        </p>
      ) : (
        <ul className="divide-y">
          {passkeys.data.items.map((p) => (
            <PasskeyRow key={p.id} passkey={p} />
          ))}
        </ul>
      )}
    </Section>
  );
}

function SessionRow({ session }: { session: Schema<"Session"> }) {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const endSession = useEndSession();
  const revoke = useMutation({
    mutationFn: () =>
      call(
        api.client.DELETE("/api/v1/me/sessions/{sessionId}", {
          params: { path: { sessionId: session.id } },
        }),
      ),
    onSuccess: async () => {
      if (session.current) {
        await endSession("signedOut");
        return;
      }
      await queryClient.invalidateQueries({ queryKey: sessionsKey });
    },
  });
  const Icon = session.device.platform === "web" ? Laptop : Smartphone;
  // Sessions of the same browser differ only in time: show it to the minute,
  // with the time zone.
  const created = exactTime(intl, session.createdAt);
  return (
    <Row
      icon={<Icon className="size-5" aria-hidden="true" />}
      actions={
        <Button
          variant="outline"
          size="sm"
          loading={revoke.isPending}
          aria-label={intl.formatMessage(messages.signOutSession, {
            device: session.device.name,
            created,
          })}
          onClick={() => revoke.mutate()}
        >
          <FormattedMessage {...messages.signOut} />
        </Button>
      }
    >
      <p className="flex flex-wrap items-center gap-2 font-medium text-sm">
        <span className="truncate">{session.device.name}</span>
        {session.current && (
          <Badge variant="success">
            <FormattedMessage {...messages.thisDevice} />
          </Badge>
        )}
      </p>
      <p className="mt-0.5 text-muted-foreground text-[13px]">
        <FormattedMessage
          {...messages.sessionSignedIn}
          values={{
            created,
            method: session.authMethods[0] ?? "password",
            time: (chunks) => <time dateTime={session.createdAt}>{chunks}</time>,
          }}
        />
      </p>
      <p className="text-muted-foreground text-[13px]">
        <FormattedMessage
          {...messages.sessionActive}
          values={{
            lastSeen: relativeTime(intl, session.lastSeenAt),
            time: (chunks) => (
              <time dateTime={session.lastSeenAt} title={exactTime(intl, session.lastSeenAt)}>
                {chunks}
              </time>
            ),
          }}
        />
      </p>
      {revoke.isError && (
        <p className="mt-1 text-destructive-subtle-foreground text-[13px]">
          {errorMessage(intl, revoke.error)}
        </p>
      )}
    </Row>
  );
}

function Sessions() {
  const api = useApi();
  const sessions = useQuery({
    queryKey: sessionsKey,
    queryFn: () => call(api.client.GET("/api/v1/me/sessions")),
  });
  // The current session first, then the most recently active.
  const items = [...(sessions.data?.items ?? [])].sort(
    (a, b) => Number(b.current) - Number(a.current) || b.lastSeenAt.localeCompare(a.lastSeenAt),
  );
  return (
    <Section
      title={<FormattedMessage {...messages.sessions} />}
      description={<FormattedMessage {...messages.sessionsDescription} />}
    >
      {sessions.isPending ? (
        <LoadingRows />
      ) : sessions.isError ? (
        <LoadError onRetry={() => sessions.refetch()} />
      ) : (
        <ul className="divide-y">
          {items.map((s) => (
            <SessionRow key={s.id} session={s} />
          ))}
        </ul>
      )}
    </Section>
  );
}

export function SecurityPage() {
  const intl = useIntl();
  return (
    <Page
      title={intl.formatMessage(messages.title)}
      description={<FormattedMessage {...messages.description} />}
    >
      <div className="flex flex-col gap-6">
        <Passkeys />
        <Sessions />
      </div>
    </Page>
  );
}
