import { useMutation, useQuery } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { AuthLayout } from "@/components/auth-layout";
import { Field } from "@/components/field";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { call } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";
import { useAfterSignIn } from "@/lib/session";

const messages = defineMessages({
  title: { id: "inviteAccept.title", defaultMessage: "Join your workspace" },
  welcome: defineMessage<{ name: string }>({
    id: "inviteAccept.welcome",
    defaultMessage: "Welcome, {name}. Set a password to get started.",
  }),
  identity: defineMessage<{ email: string }>({
    id: "inviteAccept.identity",
    defaultMessage: "Your work email is {email}. Use it to sign in.",
  }),
  password: { id: "inviteAccept.password", defaultMessage: "Password" },
  passwordHint: {
    id: "inviteAccept.passwordHint",
    defaultMessage: "At least 12 characters. A few unrelated words work well.",
  },
  accept: { id: "inviteAccept.accept", defaultMessage: "Accept invitation" },
  loading: { id: "inviteAccept.loading", defaultMessage: "Checking your invitation…" },
  invalid: {
    id: "inviteAccept.invalid",
    defaultMessage: "This invitation link is not valid. Ask an admin for a new link.",
  },
  expired: {
    id: "inviteAccept.expired",
    defaultMessage: "This invitation has expired. Ask an admin to resend it.",
  },
  used: {
    id: "inviteAccept.used",
    defaultMessage: "This invitation was already accepted. Sign in with your work email.",
  },
  short: {
    id: "inviteAccept.short",
    defaultMessage: "Choose a password with at least 12 characters.",
  },
  signIn: { id: "inviteAccept.signIn", defaultMessage: "Go to sign-in" },
});

/** Tokens stay in the URL fragment and request bodies, away from access logs. */
export function AcceptInvitationPage() {
  const intl = useIntl();
  const api = useApi();
  const afterSignIn = useAfterSignIn();
  const token = useRouterState({ select: (s) => s.location.hash.replace(/^#/, "") });
  const [password, setPassword] = useState("");
  useDocumentTitle(intl.formatMessage(messages.title));
  const preview = useQuery({
    queryKey: ["invitation-preview", token],
    queryFn: () => call(api.client.POST("/api/v1/auth/invitation", { body: { token } })),
    retry: false,
    enabled: !!token,
  });
  const accept = useMutation({
    mutationFn: () =>
      call(api.client.POST("/api/v1/auth/invitation/accept", { body: { token, password } })),
    onSuccess: async (result) => {
      await afterSignIn(result, {});
    },
  });
  const error = !token
    ? intl.formatMessage(messages.invalid)
    : preview.error || accept.error
      ? errorMessage(intl, accept.error ?? preview.error, {
          400: messages.short,
          404: messages.invalid,
          409: messages.used,
          410: messages.expired,
        })
      : undefined;

  return (
    <AuthLayout>
      <div className="rounded-xl border bg-surface p-6 shadow-lg sm:p-8">
        <h1 className="font-semibold text-2xl">
          <FormattedMessage {...messages.title} />
        </h1>
        {preview.isFetching && (
          <p role="status" className="mt-4">
            <FormattedMessage {...messages.loading} />
          </p>
        )}
        {error && <Alert className="mt-4">{error}</Alert>}
        {preview.data && (
          <>
            <p className="mt-3 text-muted-foreground">
              <FormattedMessage {...messages.welcome} values={{ name: preview.data.displayName }} />
            </p>
            <p className="mt-3 break-words text-sm">
              <FormattedMessage {...messages.identity} values={{ email: preview.data.workEmail }} />
            </p>
            <form
              className="mt-6 space-y-5"
              onSubmit={(e) => {
                e.preventDefault();
                accept.mutate();
              }}
            >
              <Field
                label={intl.formatMessage(messages.password)}
                hint={intl.formatMessage(messages.passwordHint)}
                type="password"
                autoComplete="new-password"
                required
                minLength={12}
                maxLength={1024}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              <Button className="w-full" type="submit" loading={accept.isPending}>
                <FormattedMessage {...messages.accept} />
              </Button>
            </form>
          </>
        )}
        {(!token || preview.isError) && (
          <a className="mt-5 inline-block underline" href="/sign-in">
            <FormattedMessage {...messages.signIn} />
          </a>
        )}
      </div>
    </AuthLayout>
  );
}
