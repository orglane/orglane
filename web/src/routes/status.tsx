import { Link, useRouter } from "@tanstack/react-router";
import { CloudOff, Compass } from "lucide-react";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Logo } from "@/components/logo";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { useDocumentTitle } from "@/lib/document-title";
import { errorMessage } from "@/lib/errors";

const messages = defineMessages({
  notFoundTitle: { id: "notFound.title", defaultMessage: "Page not found" },
  notFoundDescription: {
    id: "notFound.description",
    defaultMessage: "The link may be broken, or the page may have moved.",
  },
  home: { id: "notFound.home", defaultMessage: "Go home" },
  errorTitle: { id: "error.title", defaultMessage: "Something went wrong" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
  loading: { id: "common.loading", defaultMessage: "Loading" },
});

function StatusScreen({
  icon: Icon,
  title,
  description,
  children,
}: {
  icon: typeof Compass;
  title: React.ReactNode;
  description: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="auth-backdrop flex min-h-dvh flex-col items-center justify-center gap-8 px-4 text-center">
      <Logo />
      <div className="flex max-w-sm animate-fade-in flex-col items-center">
        <div className="mb-5 flex size-12 items-center justify-center rounded-xl border bg-surface text-muted-foreground shadow-sm">
          <Icon className="size-6" aria-hidden="true" />
        </div>
        <h1 className="font-semibold text-xl tracking-tight">{title}</h1>
        <p className="mt-2 text-muted-foreground text-sm leading-relaxed">{description}</p>
        <div className="mt-6">{children}</div>
      </div>
    </div>
  );
}

export function NotFoundPage() {
  const intl = useIntl();
  useDocumentTitle(intl.formatMessage(messages.notFoundTitle));
  return (
    <StatusScreen
      icon={Compass}
      title={<FormattedMessage {...messages.notFoundTitle} />}
      description={<FormattedMessage {...messages.notFoundDescription} />}
    >
      <Button asChild>
        <Link to="/">
          <FormattedMessage {...messages.home} />
        </Link>
      </Button>
    </StatusScreen>
  );
}

export function ErrorPage({ error }: { error: unknown }) {
  const intl = useIntl();
  const router = useRouter();
  useDocumentTitle(intl.formatMessage(messages.errorTitle));
  return (
    <StatusScreen
      icon={CloudOff}
      title={<FormattedMessage {...messages.errorTitle} />}
      description={errorMessage(intl, error)}
    >
      <Button onClick={() => router.invalidate()}>
        <FormattedMessage {...messages.retry} />
      </Button>
    </StatusScreen>
  );
}

export function PendingPage() {
  const intl = useIntl();
  return (
    <div
      className="flex min-h-dvh items-center justify-center text-muted-foreground"
      role="status"
      aria-label={intl.formatMessage(messages.loading)}
    >
      <Spinner className="size-6" />
    </div>
  );
}
