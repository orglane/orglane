import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Users } from "lucide-react";
import { useEffect, useState } from "react";
import { FormattedMessage, useIntl } from "react-intl";

import { Choice } from "@/components/choice";
import { EmptyState } from "@/components/empty-state";
import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Avatar } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { call } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { peopleMessages as m, peopleKey } from "@/lib/people";
import { useMe } from "@/lib/session";
import { teamsQuery } from "@/lib/teams";

export function PeoplePage() {
  const api = useApi();
  const me = useMe();
  const intl = useIntl();
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [team, setTeam] = useState("");
  const [role, setRole] = useState("");
  const [cursors, setCursors] = useState<(string | undefined)[]>([undefined]);
  useEffect(() => {
    const timer = window.setTimeout(() => {
      setQuery(search.trim());
      setCursors([undefined]);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [search]);
  const cursor = cursors.at(-1);
  const people = useQuery({
    queryKey: [...peopleKey, "directory", { query, team, role, cursor }],
    queryFn: () =>
      call(
        api.client.GET("/api/v1/people", {
          params: {
            query: {
              q: query,
              ...(team ? { team } : {}),
              ...(role ? { role } : {}),
              ...(cursor ? { cursor } : {}),
              limit: 24,
            },
          },
        }),
      ),
  });
  const teams = useQuery(teamsQuery(api));
  function clear() {
    setSearch("");
    setQuery("");
    setTeam("");
    setRole("");
    setCursors([undefined]);
  }
  return (
    <Page
      title={intl.formatMessage(m.title)}
      description={<FormattedMessage {...m.description} />}
      actions={
        me.permissions.includes("people.manage") ? (
          <Button asChild variant="outline">
            <Link to="/people/invitations">
              <FormattedMessage {...m.invite} />
            </Link>
          </Button>
        ) : undefined
      }
    >
      <Card className="mb-6 grid gap-4 p-4 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)] sm:p-5">
        <Field
          label={intl.formatMessage(m.search)}
          hint={intl.formatMessage(m.searchHint)}
          value={search}
          maxLength={200}
          type="search"
          onChange={(e) => setSearch(e.target.value)}
        />
        <Choice
          label={intl.formatMessage(m.team)}
          value={team}
          onChange={(v) => {
            setTeam(v);
            setCursors([undefined]);
          }}
        >
          <option value="">{intl.formatMessage(m.allTeams)}</option>
          {teams.data?.items.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
            </option>
          ))}
        </Choice>
        <Choice
          label={intl.formatMessage(m.role)}
          value={role}
          onChange={(v) => {
            setRole(v);
            setCursors([undefined]);
          }}
        >
          <option value="">{intl.formatMessage(m.allRoles)}</option>
          {(["owner", "admin", "member"] as const).map((r) => (
            <option key={r} value={r}>
              {intl.formatMessage(m[r])}
            </option>
          ))}
        </Choice>
      </Card>
      {teams.error && (
        <Alert>
          {errorMessage(intl, teams.error)}
          <Button variant="link" onClick={() => void teams.refetch()}>
            <FormattedMessage {...m.retry} />
          </Button>
        </Alert>
      )}
      {people.isPending && (
        <p role="status">
          <FormattedMessage {...m.loading} />
        </p>
      )}
      {people.error && (
        <Alert>
          {errorMessage(intl, people.error)}
          <Button variant="link" onClick={() => void people.refetch()}>
            <FormattedMessage {...m.retry} />
          </Button>
        </Alert>
      )}
      {people.data && (
        <>
          <div
            className="mb-3 flex flex-wrap items-center justify-between gap-2 text-muted-foreground text-sm"
            aria-live="polite"
          >
            <FormattedMessage {...m.count} values={{ count: people.data.items.length }} />
            {(query || team || role) && (
              <Button variant="ghost" size="sm" onClick={clear}>
                <FormattedMessage {...m.clear} />
              </Button>
            )}
          </div>
          {people.data.items.length === 0 ? (
            <EmptyState
              icon={Users}
              title={<FormattedMessage {...m.empty} />}
              description={<FormattedMessage {...m.emptyHint} />}
            />
          ) : (
            <ul className="grid gap-3 sm:grid-cols-2" aria-label={intl.formatMessage(m.title)}>
              {people.data.items.map((p) => (
                <li key={p.id} className="min-w-0">
                  <Link
                    to="/people/$personId"
                    params={{ personId: p.id }}
                    className="flex h-full items-start gap-3 rounded-xl border bg-surface p-4 outline-none hover:border-primary/50 focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    <Avatar name={p.displayName} src={p.avatarUrl} className="size-11 text-sm" />
                    <div className="min-w-0 flex-1">
                      <h2 className="break-words font-semibold text-sm">{p.displayName}</h2>
                      <p className="mt-1 break-words text-muted-foreground text-sm">
                        {p.title || `@${p.username}`}
                      </p>
                      <p className="mt-1 break-all text-muted-foreground text-xs">{p.workEmail}</p>
                      <div className="mt-3 flex flex-wrap gap-1.5">
                        {p.teams.map((t) => (
                          <Badge key={t.id} variant="neutral">
                            {t.name}
                          </Badge>
                        ))}
                        {p.status !== "active" && (
                          <Badge variant="neutral">{intl.formatMessage(m[p.status])}</Badge>
                        )}
                      </div>
                    </div>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          <nav
            aria-label={intl.formatMessage(m.page, { page: cursors.length })}
            className="mt-6 flex flex-wrap items-center justify-between gap-3"
          >
            <Button
              variant="outline"
              disabled={cursors.length === 1}
              onClick={() => setCursors((c) => c.slice(0, -1))}
            >
              <FormattedMessage {...m.previous} />
            </Button>
            <span className="text-muted-foreground text-sm">
              <FormattedMessage {...m.page} values={{ page: cursors.length }} />
            </span>
            <Button
              variant="outline"
              disabled={!people.data.nextCursor}
              onClick={() => setCursors((c) => [...c, people.data.nextCursor])}
            >
              <FormattedMessage {...m.next} />
            </Button>
          </nav>
        </>
      )}
    </Page>
  );
}
