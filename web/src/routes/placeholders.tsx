import { Inbox } from "lucide-react";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";

import { navMessages } from "@/components/app-shell";
import { EmptyState } from "@/components/empty-state";
import { Page } from "@/components/page";

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
