import { useMutation } from "@tanstack/react-query";
import { KeyRound } from "lucide-react";
import { type FormEvent, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { AuthCard, AuthLayout } from "@/components/auth-layout";
import { Field } from "@/components/field";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { call } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { workspaceHost } from "@/lib/brand";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";
import { useAfterSignIn } from "@/lib/session";
import { getPasskey, isCancellation, passkeysSupported } from "@/lib/webauthn";

export const signInMessages = defineMessages({
  title: { id: "signIn.title", defaultMessage: "Sign in" },
  description: defineMessage<{ host: string }>({
    id: "signIn.description",
    defaultMessage: "Welcome back to {host}.",
  }),
  descriptionApp: defineMessage<{ host: string }>({
    id: "signIn.descriptionApp",
    defaultMessage: "Sign in with your {host} account to continue to the application.",
  }),
  email: { id: "signIn.email", defaultMessage: "Work email" },
  emailPlaceholder: { id: "signIn.emailPlaceholder", defaultMessage: "you@company.com" },
  password: { id: "signIn.password", defaultMessage: "Password" },
  submit: { id: "signIn.submit", defaultMessage: "Sign in" },
  or: { id: "signIn.or", defaultMessage: "or" },
  passkey: { id: "signIn.passkey", defaultMessage: "Sign in with a passkey" },
  wrongPassword: {
    id: "signIn.wrongPassword",
    defaultMessage: "The email or password is incorrect.",
  },
  passkeyRejected: {
    id: "signIn.passkeyRejected",
    defaultMessage:
      "That passkey isn't registered for this workspace. Try another one or use your password.",
  },
  passkeyFailed: {
    id: "signIn.passkeyFailed",
    defaultMessage: "Signing in with a passkey didn't work. Try again or use your password.",
  },
  signedOut: { id: "signIn.signedOut", defaultMessage: "You signed out." },
  sessionEnded: {
    id: "signIn.sessionEnded",
    defaultMessage: "Your session has ended. Sign in again to continue where you left off.",
  },
  limited: {
    id: "signIn.limited",
    defaultMessage: "Too many sign-in attempts. Wait a few minutes and try again.",
  },
  failed: { id: "signIn.failed", defaultMessage: "Signing in didn't work. Try again in a moment." },
  help: {
    id: "signIn.help",
    defaultMessage: "Forgot your password? Ask an administrator of your workspace to reset it.",
  },
});

export interface SignInSearch {
  authRequest?: string;
  email?: string;
  redirect?: string;
  signedOut?: boolean;
  /** The session ended while the app was open (expired or revoked). */
  sessionEnded?: boolean;
  /** Set by the server after a failed basic form sign-in (see basic-sign-in.js). */
  error?: "credentials" | "limited" | "failed";
}

export function SignInPage({ search }: { search: SignInSearch }) {
  const intl = useIntl();
  const api = useApi();
  const afterSignIn = useAfterSignIn();
  const [email, setEmail] = useState(search.email ?? "");
  const [password, setPassword] = useState("");
  const next = { authRequest: search.authRequest, redirect: search.redirect };
  useDocumentTitle(intl.formatMessage(signInMessages.title));

  const passwordSignIn = useMutation({
    mutationFn: () =>
      call(api.client.POST("/api/v1/auth/sign-in", { body: { email: email.trim(), password } })),
    onSuccess: (result) => afterSignIn(result, next),
    onError: () => setPassword(""),
  });

  const passkeySignIn = useMutation({
    mutationFn: async () => {
      const ceremony = await call(api.client.POST("/api/v1/auth/passkey/options"));
      const credential = await getPasskey(ceremony.publicKey);
      return call(
        api.client.POST("/api/v1/auth/passkey", {
          body: { ceremonyId: ceremony.ceremonyId, credential },
        }),
      );
    },
    onSuccess: (result) => afterSignIn(result, next),
  });

  const busy =
    passwordSignIn.isPending ||
    passkeySignIn.isPending ||
    passwordSignIn.isSuccess ||
    passkeySignIn.isSuccess;

  let error: string | undefined;
  if (search.error && passwordSignIn.isIdle && passkeySignIn.isIdle) {
    const byCode = {
      credentials: signInMessages.wrongPassword,
      limited: signInMessages.limited,
      failed: signInMessages.failed,
    };
    error = intl.formatMessage(byCode[search.error]);
  } else if (passwordSignIn.isError) {
    error = errorMessage(intl, passwordSignIn.error, { 401: signInMessages.wrongPassword });
  } else if (passkeySignIn.isError && !isCancellation(passkeySignIn.error)) {
    error =
      passkeySignIn.error instanceof DOMException
        ? intl.formatMessage(signInMessages.passkeyFailed)
        : errorMessage(intl, passkeySignIn.error, {
            401: signInMessages.passkeyRejected,
            400: signInMessages.passkeyFailed,
          });
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    passkeySignIn.reset();
    passwordSignIn.mutate();
  };

  return (
    <AuthLayout footer={<FormattedMessage {...signInMessages.help} />}>
      <AuthCard
        title={<FormattedMessage {...signInMessages.title} />}
        description={
          <FormattedMessage
            {...(search.authRequest ? signInMessages.descriptionApp : signInMessages.description)}
            values={{ host: workspaceHost() }}
          />
        }
      >
        <div className="flex flex-col gap-5">
          {search.signedOut && !error && (
            <Alert variant="success">
              <FormattedMessage {...signInMessages.signedOut} />
            </Alert>
          )}
          {search.sessionEnded && !search.signedOut && !error && (
            <Alert variant="info">
              <FormattedMessage {...signInMessages.sessionEnded} />
            </Alert>
          )}
          {error && <Alert>{error}</Alert>}
          <form className="flex flex-col gap-4" onSubmit={onSubmit} noValidate={false}>
            <Field
              id="email"
              name="email"
              type="email"
              autoComplete="username"
              inputMode="email"
              autoCapitalize="none"
              spellCheck={false}
              required
              autoFocus={!search.email}
              label={<FormattedMessage {...signInMessages.email} />}
              placeholder={intl.formatMessage(signInMessages.emailPlaceholder)}
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
            <Field
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              autoFocus={!!search.email}
              label={<FormattedMessage {...signInMessages.password} />}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <Button
              id="submit"
              type="submit"
              size="lg"
              className="mt-1 w-full"
              loading={passwordSignIn.isPending || passwordSignIn.isSuccess}
              disabled={busy}
            >
              <FormattedMessage {...signInMessages.submit} />
            </Button>
          </form>
          {passkeysSupported() && (
            <>
              <div className="flex items-center gap-3 text-muted-foreground text-xs uppercase tracking-wide">
                <span className="h-px flex-1 bg-border" />
                <FormattedMessage {...signInMessages.or} />
                <span className="h-px flex-1 bg-border" />
              </div>
              <Button
                variant="outline"
                size="lg"
                className="w-full"
                loading={passkeySignIn.isPending || passkeySignIn.isSuccess}
                disabled={busy}
                onClick={() => {
                  passwordSignIn.reset();
                  passkeySignIn.mutate();
                }}
              >
                <KeyRound />
                <FormattedMessage {...signInMessages.passkey} />
              </Button>
            </>
          )}
        </div>
      </AuthCard>
    </AuthLayout>
  );
}
