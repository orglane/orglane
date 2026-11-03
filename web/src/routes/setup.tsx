import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ChangeEvent, type FormEvent, type ReactNode, useState } from "react";
import {
  defineMessage,
  defineMessages,
  FormattedMessage,
  type MessageTag,
  useIntl,
} from "react-intl";

import { AuthCard, AuthLayout } from "@/components/auth-layout";
import { Field } from "@/components/field";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { call, hasStatus, setupStatusKey } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";
import { useAfterSignIn } from "@/lib/session";

/** Matches the server's rules (core/internal/identity). */
export const minPasswordLength = 12;
const usernamePattern = /^[a-z0-9][a-z0-9._-]{0,62}$/;
const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

const messages = defineMessages({
  title: { id: "setup.title", defaultMessage: "Set up your workspace" },
  description: {
    id: "setup.description",
    defaultMessage:
      "Create the owner account. The owner can invite people and manage everything in the workspace.",
  },
  token: { id: "setup.token", defaultMessage: "Setup token" },
  tokenHint: defineMessage<{ code: MessageTag }>({
    id: "setup.tokenHint",
    defaultMessage:
      "Printed in the server log on first start. To make a new one, run <code>orglane cli setup-token</code>.",
  }),
  name: { id: "setup.name", defaultMessage: "Full name" },
  namePlaceholder: { id: "setup.namePlaceholder", defaultMessage: "Alex Morgan" },
  email: { id: "setup.email", defaultMessage: "Work email" },
  emailPlaceholder: { id: "setup.emailPlaceholder", defaultMessage: "alex@company.com" },
  username: { id: "setup.username", defaultMessage: "Username" },
  usernameHint: {
    id: "setup.usernameHint",
    defaultMessage:
      "Lowercase letters, digits, dots, dashes and underscores. Used in services such as Git.",
  },
  password: { id: "setup.password", defaultMessage: "Password" },
  passwordHint: defineMessage<{ min: number }>({
    id: "setup.passwordHint",
    defaultMessage: "At least {min} characters. A passphrase of a few words works well.",
  }),
  submit: { id: "setup.submit", defaultMessage: "Create owner account" },
  required: { id: "setup.error.required", defaultMessage: "Fill this in." },
  invalidEmail: {
    id: "setup.error.email",
    defaultMessage: "Enter an email address like name@company.com.",
  },
  invalidUsername: {
    id: "setup.error.username",
    defaultMessage:
      "Start with a letter or digit and use only lowercase letters, digits, dots, dashes and underscores.",
  },
  shortPassword: defineMessage<{ min: number }>({
    id: "setup.error.password",
    defaultMessage: "Use at least {min} characters.",
  }),
  invalidToken: {
    id: "setup.error.token",
    defaultMessage:
      "This setup token isn't valid. Copy it again from the server log, or create a new one.",
  },
  alreadyDone: { id: "setup.error.done", defaultMessage: "This workspace is already set up." },
  signIn: { id: "setup.signIn", defaultMessage: "Go to sign in" },
  rejected: {
    id: "setup.error.rejected",
    defaultMessage: "The server didn't accept these details. Check the fields and try again.",
  },
});

type Fields = "setupToken" | "displayName" | "workEmail" | "username" | "password";

/** A username suggestion from an email address: "Alex.Morgan@acme.com" → "alex.morgan". */
export function suggestUsername(email: string): string {
  const local = email.split("@")[0]?.toLowerCase() ?? "";
  return local
    .replace(/[^a-z0-9._-]/g, "")
    .replace(/^[._-]+/, "")
    .slice(0, 63);
}

export function SetupPage() {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const afterSignIn = useAfterSignIn();
  useDocumentTitle(intl.formatMessage(messages.title));

  const [values, setValues] = useState<Record<Fields, string>>({
    setupToken: "",
    displayName: "",
    workEmail: "",
    username: "",
    password: "",
  });
  const [usernameEdited, setUsernameEdited] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  const set = (field: Fields) => (e: ChangeEvent<HTMLInputElement>) => {
    const value = e.target.value;
    setValues((v) => {
      const next = { ...v, [field]: value };
      if (field === "workEmail" && !usernameEdited) {
        next.username = suggestUsername(value);
      }
      return next;
    });
    if (field === "username") {
      setUsernameEdited(true);
    }
  };

  const errors: Partial<Record<Fields, string>> = {};
  for (const f of Object.keys(values) as Fields[]) {
    if (values[f].trim() === "") {
      errors[f] = intl.formatMessage(messages.required);
    }
  }
  if (!errors.workEmail && !emailPattern.test(values.workEmail.trim())) {
    errors.workEmail = intl.formatMessage(messages.invalidEmail);
  }
  if (!errors.username && !usernamePattern.test(values.username)) {
    errors.username = intl.formatMessage(messages.invalidUsername);
  }
  if (!errors.password && [...values.password].length < minPasswordLength) {
    errors.password = intl.formatMessage(messages.shortPassword, { min: minPasswordLength });
  }

  const setup = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/auth/setup", {
          body: {
            setupToken: values.setupToken.trim(),
            displayName: values.displayName.trim(),
            workEmail: values.workEmail.trim(),
            username: values.username,
            password: values.password,
          },
        }),
      ),
    onSuccess: async (result) => {
      queryClient.setQueryData(setupStatusKey, { required: false });
      await afterSignIn(result, {});
    },
    onError: (err) => {
      if (hasStatus(err, 409)) {
        queryClient.setQueryData(setupStatusKey, { required: false });
      }
    },
  });

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (Object.keys(errors).length > 0) {
      const first = (Object.keys(values) as Fields[]).find((f) => errors[f]);
      document.getElementById(`setup-${first}`)?.focus();
      return;
    }
    setup.mutate();
  };

  const shown = (f: Fields) => (submitted ? errors[f] : undefined);
  const tokenError = hasStatus(setup.error, 403)
    ? intl.formatMessage(messages.invalidToken)
    : shown("setupToken");

  let formError: ReactNode;
  if (setup.isError && !hasStatus(setup.error, 403)) {
    formError = hasStatus(setup.error, 409) ? (
      <span className="flex flex-wrap items-center justify-between gap-2">
        <FormattedMessage {...messages.alreadyDone} />
        <Link to="/sign-in" className="font-medium underline underline-offset-4">
          <FormattedMessage {...messages.signIn} />
        </Link>
      </span>
    ) : (
      errorMessage(intl, setup.error, { 400: messages.rejected })
    );
  }

  return (
    <AuthLayout>
      <AuthCard
        title={<FormattedMessage {...messages.title} />}
        description={<FormattedMessage {...messages.description} />}
      >
        <form className="flex flex-col gap-4" onSubmit={onSubmit} noValidate>
          {formError && <Alert>{formError}</Alert>}
          <Field
            id="setup-setupToken"
            name="setupToken"
            autoComplete="one-time-code"
            autoCapitalize="none"
            spellCheck={false}
            autoFocus
            className="[&_input]:font-mono"
            label={<FormattedMessage {...messages.token} />}
            hint={
              <FormattedMessage
                {...messages.tokenHint}
                values={{
                  code: (chunks) => (
                    <code className="rounded bg-muted px-1 py-0.5 font-mono text-[12px]">
                      {chunks}
                    </code>
                  ),
                }}
              />
            }
            error={tokenError}
            value={values.setupToken}
            onChange={set("setupToken")}
          />
          <div className="my-1 h-px bg-border" />
          <Field
            id="setup-displayName"
            name="name"
            autoComplete="name"
            label={<FormattedMessage {...messages.name} />}
            placeholder={intl.formatMessage(messages.namePlaceholder)}
            error={shown("displayName")}
            value={values.displayName}
            onChange={set("displayName")}
          />
          <Field
            id="setup-workEmail"
            name="email"
            type="email"
            inputMode="email"
            autoComplete="email"
            autoCapitalize="none"
            spellCheck={false}
            label={<FormattedMessage {...messages.email} />}
            placeholder={intl.formatMessage(messages.emailPlaceholder)}
            error={shown("workEmail")}
            value={values.workEmail}
            onChange={set("workEmail")}
          />
          <Field
            id="setup-username"
            name="username"
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            label={<FormattedMessage {...messages.username} />}
            hint={<FormattedMessage {...messages.usernameHint} />}
            error={shown("username")}
            value={values.username}
            onChange={set("username")}
          />
          <Field
            id="setup-password"
            name="new-password"
            type="password"
            autoComplete="new-password"
            label={<FormattedMessage {...messages.password} />}
            hint={
              <FormattedMessage {...messages.passwordHint} values={{ min: minPasswordLength }} />
            }
            error={shown("password")}
            value={values.password}
            onChange={set("password")}
          />
          <Button
            type="submit"
            size="lg"
            className="mt-2 w-full"
            loading={setup.isPending || setup.isSuccess}
          >
            <FormattedMessage {...messages.submit} />
          </Button>
        </form>
      </AuthCard>
    </AuthLayout>
  );
}
