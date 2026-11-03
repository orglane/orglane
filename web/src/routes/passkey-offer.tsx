import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { CircleCheck, Fingerprint, KeyRound, ShieldCheck, Zap } from "lucide-react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { AuthLayout } from "@/components/auth-layout";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { call, meKey } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";
import { useAddPasskey } from "@/lib/passkeys";
import { useMe } from "@/lib/session";
import { safeRedirect } from "@/lib/utils";
import { isCancellation } from "@/lib/webauthn";

const messages = defineMessages({
  title: { id: "passkeyOffer.title", defaultMessage: "Sign in faster with a passkey" },
  description: defineMessage<{ name: string }>({
    id: "passkeyOffer.description",
    defaultMessage:
      "Use your fingerprint, face or screen lock instead of typing your password, {name}.",
  }),
  fast: {
    id: "passkeyOffer.benefit.fast",
    defaultMessage: "One touch to sign in, on this device or your phone.",
  },
  safe: {
    id: "passkeyOffer.benefit.safe",
    defaultMessage: "Can't be phished, guessed or leaked like a password.",
  },
  private: {
    id: "passkeyOffer.benefit.private",
    defaultMessage: "Your fingerprint or face never leaves your device.",
  },
  add: { id: "passkeyOffer.add", defaultMessage: "Add a passkey" },
  skip: { id: "passkeyOffer.skip", defaultMessage: "Skip for now" },
  later: {
    id: "passkeyOffer.later",
    defaultMessage: "You can add one any time in Sign-in & security.",
  },
  added: { id: "passkeyOffer.added", defaultMessage: "Passkey added" },
  addedDescription: {
    id: "passkeyOffer.addedDescription",
    defaultMessage: "Next time, sign in with a passkey instead of your password.",
  },
  continue: { id: "passkeyOffer.continue", defaultMessage: "Continue" },
  failed: {
    id: "passkeyOffer.failed",
    defaultMessage: "The passkey couldn't be created. Try again, or skip for now.",
  },
});

const benefits = [
  { icon: Zap, message: messages.fast },
  { icon: ShieldCheck, message: messages.safe },
  { icon: Fingerprint, message: messages.private },
];

/** Shown once after sign-in to people without a passkey, with a clear way out. */
export function PasskeyOfferPage({ redirect }: { redirect?: string | undefined }) {
  const intl = useIntl();
  const api = useApi();
  const me = useMe();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const target = safeRedirect(redirect);
  useDocumentTitle(intl.formatMessage(messages.title));

  const add = useAddPasskey();
  const skip = useMutation({
    mutationFn: () => call(api.client.POST("/api/v1/me/passkey-offer/dismiss")),
    onSettled: async () => {
      // Skipping must never trap the person here, even if remembering it failed.
      queryClient.setQueryData(meKey, (old: typeof me | null | undefined) =>
        old ? { ...old, passkeyOffer: false } : old,
      );
      await navigate({ href: target, replace: true });
    },
  });

  if (add.isSuccess) {
    return (
      <AuthLayout>
        <div className="rounded-xl border bg-surface p-6 text-center shadow-lg sm:p-8">
          <div className="mx-auto mb-5 flex size-14 items-center justify-center rounded-full bg-success-subtle text-success-subtle-foreground">
            <CircleCheck className="size-7" aria-hidden="true" />
          </div>
          <h1 className="font-semibold text-2xl tracking-tight">
            <FormattedMessage {...messages.added} />
          </h1>
          <p className="mt-2 text-muted-foreground text-sm">
            <FormattedMessage {...messages.addedDescription} />
          </p>
          <Button
            size="lg"
            className="mt-6 w-full"
            autoFocus
            onClick={() => navigate({ href: target, replace: true })}
          >
            <FormattedMessage {...messages.continue} />
          </Button>
        </div>
      </AuthLayout>
    );
  }

  const error =
    add.isError && !isCancellation(add.error)
      ? add.error instanceof DOMException
        ? intl.formatMessage(messages.failed)
        : errorMessage(intl, add.error, { 400: messages.failed })
      : undefined;

  return (
    <AuthLayout>
      <div className="rounded-xl border bg-surface p-6 shadow-lg sm:p-8">
        <div className="mb-6 flex size-14 items-center justify-center rounded-2xl bg-primary-subtle text-primary-subtle-foreground">
          <KeyRound className="size-7" aria-hidden="true" strokeWidth={1.75} />
        </div>
        <h1 className="font-semibold text-2xl tracking-tight">
          <FormattedMessage {...messages.title} />
        </h1>
        <p className="mt-2 text-muted-foreground text-sm leading-relaxed">
          <FormattedMessage
            {...messages.description}
            values={{ name: me.person.displayName.split(" ")[0] ?? me.person.displayName }}
          />
        </p>
        <ul className="mt-6 flex flex-col gap-3">
          {benefits.map(({ icon: Icon, message }) => (
            <li key={message.id} className="flex items-start gap-3 text-sm">
              <span className="mt-px flex size-6 shrink-0 items-center justify-center rounded-md bg-muted text-foreground">
                <Icon className="size-3.5" aria-hidden="true" />
              </span>
              <span className="leading-6">
                <FormattedMessage {...message} />
              </span>
            </li>
          ))}
        </ul>
        {error && <Alert className="mt-6">{error}</Alert>}
        <div className="mt-7 flex flex-col gap-2">
          <Button
            size="lg"
            className="w-full"
            loading={add.isPending}
            disabled={skip.isPending}
            onClick={() => add.mutate()}
          >
            <FormattedMessage {...messages.add} />
          </Button>
          <Button
            variant="ghost"
            size="lg"
            className="w-full text-muted-foreground"
            loading={skip.isPending}
            disabled={add.isPending}
            onClick={() => skip.mutate()}
          >
            <FormattedMessage {...messages.skip} />
          </Button>
        </div>
        <p className="mt-4 text-center text-muted-foreground text-xs">
          <FormattedMessage {...messages.later} />
        </p>
      </div>
    </AuthLayout>
  );
}
