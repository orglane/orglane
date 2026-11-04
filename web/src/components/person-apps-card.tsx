import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { useApi } from "@/lib/api-context";
import { personAppsQuery, statusMessages, statusVariant } from "@/lib/apps";
import { errorMessage } from "@/lib/errors";

const messages = defineMessages({
  title: { id: "personApps.title", defaultMessage: "Apps and accounts" },
  description: {
    id: "personApps.description",
    defaultMessage: "This person's access in your company's apps and their linked accounts.",
  },
  none: { id: "personApps.none", defaultMessage: "No app access or linked accounts yet." },
  noAccess: { id: "personApps.noAccess", defaultMessage: "No access" },
  verified: { id: "personApps.verified", defaultMessage: "Verified" },
  pending: { id: "personApps.pending", defaultMessage: "Expected, not verified" },
  pendingHint: {
    id: "personApps.pendingHint",
    defaultMessage: "From an import. It proves nothing until the person links the account.",
  },
  links: { id: "personApps.links", defaultMessage: "Linked accounts" },
  access: { id: "personApps.access", defaultMessage: "Access" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
});

/** A person's app access and links, for people managers (person page). */
export function PersonAppsCard({
  personId,
  manageApps,
}: {
  personId: string;
  manageApps: boolean;
}) {
  const api = useApi();
  const intl = useIntl();
  const apps = useQuery(personAppsQuery(api, personId));
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <FormattedMessage {...messages.title} />
        </CardTitle>
        <CardDescription>
          <FormattedMessage {...messages.description} />
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {apps.isError ? (
          <Alert>
            <p>{errorMessage(intl, apps.error)}</p>
            <Button variant="link" onClick={() => void apps.refetch()}>
              <FormattedMessage {...messages.retry} />
            </Button>
          </Alert>
        ) : !apps.data ? null : apps.data.results.length === 0 && apps.data.links.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            <FormattedMessage {...messages.none} />
          </p>
        ) : (
          <>
            {apps.data.results.length > 0 && (
              <section aria-label={intl.formatMessage(messages.access)}>
                <ul className="divide-y text-sm">
                  {apps.data.results.map((r) => (
                    <li
                      key={r.instanceId}
                      className="flex flex-wrap items-center justify-between gap-2 py-2"
                    >
                      {manageApps ? (
                        <Link
                          to="/settings/apps/$instanceId"
                          params={{ instanceId: r.instanceId }}
                          className="font-medium text-primary hover:underline"
                        >
                          {r.instanceDisplayName}
                        </Link>
                      ) : (
                        <span className="font-medium">{r.instanceDisplayName}</span>
                      )}
                      <span className="flex flex-wrap items-center gap-1">
                        {r.access ? (
                          <Badge variant={statusVariant(r.status)}>
                            {intl.formatMessage(statusMessages[r.status])}
                          </Badge>
                        ) : (
                          <Badge>
                            <FormattedMessage {...messages.noAccess} />
                          </Badge>
                        )}
                        {r.errorCode && (
                          <code className="font-mono text-muted-foreground text-xs">
                            {r.errorCode}
                          </code>
                        )}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
            )}
            {apps.data.links.length > 0 && (
              <section aria-label={intl.formatMessage(messages.links)}>
                <h3 className="mb-1 font-medium text-sm">
                  <FormattedMessage {...messages.links} />
                </h3>
                <ul className="divide-y text-sm">
                  {apps.data.links.map((l) => (
                    <li
                      key={`${l.state}-${l.provider}-${l.realm ?? l.organizationId}-${l.externalId}`}
                      className="flex flex-wrap items-center justify-between gap-2 py-2"
                    >
                      <span className="min-w-0 break-all">
                        <span className="font-medium">@{l.login}</span>
                        <span className="ml-2 text-muted-foreground">{l.provider}</span>
                      </span>
                      {l.state === "verified" ? (
                        <Badge variant="success">
                          <FormattedMessage {...messages.verified} />
                        </Badge>
                      ) : (
                        <Badge variant="warning" title={intl.formatMessage(messages.pendingHint)}>
                          <FormattedMessage {...messages.pending} />
                        </Badge>
                      )}
                    </li>
                  ))}
                </ul>
              </section>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
