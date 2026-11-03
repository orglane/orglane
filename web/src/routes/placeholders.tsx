import { Inbox, LayoutGrid } from "lucide-react";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";

import { navMessages } from "@/components/app-shell";
import { EmptyState } from "@/components/empty-state";
import { Page } from "@/components/page";
import { Badge } from "@/components/ui/badge";

const messages = defineMessages({
  inboxDescription: {
    id: "inbox.description",
    defaultMessage: "Everything that needs your attention, in one place.",
  },
  inboxEmptyTitle: { id: "inbox.empty.title", defaultMessage: "You're all caught up" },
  inboxEmptyDescription: {
    id: "inbox.empty.description",
    defaultMessage: "Mentions, reviews and updates from your company's tools will arrive here.",
  },
  appsDescription: {
    id: "apps.description",
    defaultMessage: "The services your company uses, with one sign-in.",
  },
  appsEmptyTitle: { id: "apps.empty.title", defaultMessage: "No apps connected yet" },
  appsEmptyDescription: {
    id: "apps.empty.description",
    defaultMessage:
      "Code hosting, chat, files and calendars will show up here once they're connected.",
  },
  soon: { id: "common.comingSoon", defaultMessage: "Coming soon" },
});

export function InboxPage() {
  const intl = useIntl();
  return (
    <Page
      title={intl.formatMessage(navMessages.inbox)}
      description={<FormattedMessage {...messages.inboxDescription} />}
    >
      <EmptyState
        icon={Inbox}
        title={<FormattedMessage {...messages.inboxEmptyTitle} />}
        description={<FormattedMessage {...messages.inboxEmptyDescription} />}
      />
    </Page>
  );
}

export function AppsPage() {
  const intl = useIntl();
  return (
    <Page
      title={intl.formatMessage(navMessages.apps)}
      description={<FormattedMessage {...messages.appsDescription} />}
    >
      <EmptyState
        icon={LayoutGrid}
        title={<FormattedMessage {...messages.appsEmptyTitle} />}
        description={<FormattedMessage {...messages.appsEmptyDescription} />}
      >
        <Badge variant="brand">
          <FormattedMessage {...messages.soon} />
        </Badge>
      </EmptyState>
    </Page>
  );
}
