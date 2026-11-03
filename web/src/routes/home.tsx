import { Link } from "@tanstack/react-router";
import { ArrowRight, KeyRound, LayoutGrid, Sparkles } from "lucide-react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";
import { EmptyState } from "@/components/empty-state";
import { OnboardingChecklist } from "@/components/onboarding-checklist";
import { Page } from "@/components/page";
import { Card } from "@/components/ui/card";
import { useMe } from "@/lib/session";

const messages = defineMessages({
  title: { id: "home.title", defaultMessage: "Home" },
  greeting: defineMessage<{ period: string; name: string }>({
    id: "home.greeting",
    defaultMessage:
      "{period, select, morning {Good morning} afternoon {Good afternoon} other {Good evening}}, {name}",
  }),
  description: {
    id: "home.description",
    defaultMessage: "Here's what's happening in your workspace.",
  },
  emptyTitle: { id: "home.empty.title", defaultMessage: "Your home is taking shape" },
  emptyDescription: {
    id: "home.empty.description",
    defaultMessage:
      "Widgets from your company's tools will live here: pull requests to review, tasks, meetings and news.",
  },
  securityTitle: { id: "home.security.title", defaultMessage: "Secure your account" },
  securityDescription: {
    id: "home.security.description",
    defaultMessage: "Add a passkey to sign in with your fingerprint, face or screen lock.",
  },
  appsTitle: { id: "home.apps.title", defaultMessage: "Your company's apps" },
  appsDescription: {
    id: "home.apps.description",
    defaultMessage: "Code, chat, files and more, with one sign-in.",
  },
});

function period(hour: number): "morning" | "afternoon" | "evening" {
  if (hour >= 5 && hour < 12) {
    return "morning";
  }
  return hour >= 12 && hour < 18 ? "afternoon" : "evening";
}

function Shortcut({
  to,
  icon: Icon,
  title,
  description,
}: {
  to: "/settings/security" | "/apps";
  icon: typeof KeyRound;
  title: React.ReactNode;
  description: React.ReactNode;
}) {
  return (
    <Link
      to={to}
      className="group rounded-xl outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background"
    >
      <Card className="flex h-full items-start gap-4 p-5 shadow-xs transition-[border-color,box-shadow] group-hover:border-input group-hover:shadow-sm">
        <span className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary-subtle text-primary-subtle-foreground">
          <Icon className="size-5" aria-hidden="true" strokeWidth={1.9} />
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5 font-medium text-sm">
            {title}
            <ArrowRight
              className="size-3.5 text-muted-foreground transition-transform group-hover:translate-x-0.5"
              aria-hidden="true"
            />
          </span>
          <span className="mt-1 block text-muted-foreground text-sm">{description}</span>
        </span>
      </Card>
    </Link>
  );
}

export function HomePage() {
  const intl = useIntl();
  const me = useMe();
  const name = me.person.displayName.split(" ")[0] ?? me.person.displayName;
  return (
    <Page
      documentTitle={intl.formatMessage(messages.title)}
      title={
        <FormattedMessage
          {...messages.greeting}
          values={{ period: period(new Date().getHours()), name }}
        />
      }
      description={<FormattedMessage {...messages.description} />}
    >
      <div className="flex flex-col gap-6">
        <OnboardingChecklist />
        <EmptyState
          icon={Sparkles}
          title={<FormattedMessage {...messages.emptyTitle} />}
          description={<FormattedMessage {...messages.emptyDescription} />}
        />
        <div className="grid gap-4 sm:grid-cols-2">
          <Shortcut
            to="/settings/security"
            icon={KeyRound}
            title={<FormattedMessage {...messages.securityTitle} />}
            description={<FormattedMessage {...messages.securityDescription} />}
          />
          <Shortcut
            to="/apps"
            icon={LayoutGrid}
            title={<FormattedMessage {...messages.appsTitle} />}
            description={<FormattedMessage {...messages.appsDescription} />}
          />
        </div>
      </div>
    </Page>
  );
}
