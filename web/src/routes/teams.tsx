import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Plus, UsersRound } from "lucide-react";
import { type FormEvent, useId, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { EmptyState } from "@/components/empty-state";
import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Avatar } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { call, hasStatus, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";
import { teamMembersQuery, teamQuery, teamsKey, teamsQuery } from "@/lib/teams";

const messages = defineMessages({
  title: { id: "teams.title", defaultMessage: "Teams" },
  description: {
    id: "teams.description",
    defaultMessage: "Find the people working together across your company.",
  },
  search: { id: "teams.search", defaultMessage: "Search teams" },
  showArchived: { id: "teams.showArchived", defaultMessage: "Include archived teams" },
  newTeam: { id: "teams.new", defaultMessage: "New team" },
  create: { id: "teams.create", defaultMessage: "Create team" },
  edit: { id: "teams.edit", defaultMessage: "Edit team" },
  save: { id: "teams.save", defaultMessage: "Save changes" },
  name: { id: "teams.name", defaultMessage: "Team name" },
  slug: { id: "teams.slug", defaultMessage: "Slug" },
  slugHint: {
    id: "teams.slugHint",
    defaultMessage: "Use letters, numbers and hyphens, for example platform.",
  },
  teamDescription: { id: "teams.teamDescription", defaultMessage: "Description" },
  empty: { id: "teams.empty", defaultMessage: "No teams yet" },
  noMatches: { id: "teams.noMatches", defaultMessage: "No teams found" },
  emptyDescription: {
    id: "teams.emptyDescription",
    defaultMessage: "Teams bring people together around their work.",
  },
  count: defineMessage<{ count: number }>({
    id: "teams.count",
    defaultMessage: "{count, plural, one {# member} other {# members}}",
  }),
  archived: { id: "teams.archived", defaultMessage: "Archived" },
  archivedInfo: {
    id: "teams.archivedInfo",
    defaultMessage: "This team is archived. Its members and history are kept.",
  },
  archive: { id: "teams.archive", defaultMessage: "Archive team" },
  archiveQuestion: {
    id: "teams.archiveQuestion",
    defaultMessage:
      "Archive this team? It will become read-only and leave the active teams list. Members and history are kept.",
  },
  confirmArchive: { id: "teams.confirmArchive", defaultMessage: "Confirm archive" },
  archiveConflict: {
    id: "teams.archiveConflict",
    defaultMessage: "This team has active child teams. Move or archive them first.",
  },
  conflict: { id: "teams.conflict", defaultMessage: "Another team already uses this slug." },
  invalid: {
    id: "teams.invalid",
    defaultMessage: "Check the team name, slug and description and try again.",
  },
  changed: {
    id: "teams.changed",
    defaultMessage: "The team or person has changed. Refresh the page and try again.",
  },
  notFound: { id: "teams.notFound", defaultMessage: "Team not found" },
  loading: { id: "common.loading", defaultMessage: "Loading" },
  retry: { id: "common.retry", defaultMessage: "Try again" },
  cancel: { id: "common.cancel", defaultMessage: "Cancel" },
  back: { id: "teams.back", defaultMessage: "All teams" },
  members: { id: "teams.members", defaultMessage: "Members" },
  noMembers: { id: "teams.noMembers", defaultMessage: "No members yet" },
  lead: { id: "teams.lead", defaultMessage: "Lead" },
  member: { id: "teams.member", defaultMessage: "Member" },
  addMember: { id: "teams.addMember", defaultMessage: "Add member" },
  personToAdd: { id: "teams.personToAdd", defaultMessage: "Person to add" },
  choosePerson: { id: "teams.choosePerson", defaultMessage: "Choose a person" },
  everyoneAdded: { id: "teams.everyoneAdded", defaultMessage: "Everyone is already in this team." },
  makeLead: defineMessage<{ name: string }>({
    id: "teams.makeLead",
    defaultMessage: "Make {name} a lead",
  }),
  makeMember: defineMessage<{ name: string }>({
    id: "teams.makeMember",
    defaultMessage: "Make {name} a member",
  }),
  makeLeadShort: { id: "teams.makeLeadShort", defaultMessage: "Make lead" },
  makeMemberShort: { id: "teams.makeMemberShort", defaultMessage: "Make member" },
  remove: defineMessage<{ name: string }>({
    id: "teams.remove",
    defaultMessage: "Remove {name} from team",
  }),
  removeShort: { id: "teams.removeShort", defaultMessage: "Remove" },
});

function Loading() {
  const intl = useIntl();
  return (
    <div role="status" aria-label={intl.formatMessage(messages.loading)} className="p-6">
      <Spinner />
    </div>
  );
}

function LoadError({ error, retry }: { error: unknown; retry: () => void }) {
  const intl = useIntl();
  return (
    <Alert>
      <p>{errorMessage(intl, error)}</p>
      <Button variant="link" onClick={retry}>
        <FormattedMessage {...messages.retry} />
      </Button>
    </Alert>
  );
}

/** Uses the existing public directory API; only managers need the picker. */
function AddMember({ teamId, members }: { teamId: string; members: Schema<"TeamMember">[] }) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const id = useId();
  const [personId, setPersonId] = useState("");
  const people = useQuery({
    queryKey: ["people"],
    queryFn: () => call(api.client.GET("/api/v1/people")),
  });
  const available =
    people.data?.items.filter((p) => !members.some((m) => m.person.id === p.id)) ?? [];
  const add = useMutation({
    mutationFn: () =>
      call(
        api.client.PUT("/api/v1/teams/{teamId}/members/{personId}", {
          params: { path: { teamId, personId } },
          body: { role: "member" },
        }),
      ),
    onSuccess: async () => {
      setPersonId("");
      await queryClient.invalidateQueries({ queryKey: teamsKey });
    },
  });
  if (people.isPending) return <Loading />;
  if (people.isError) return <LoadError error={people.error} retry={() => void people.refetch()} />;
  return (
    <form
      className="space-y-3 border-t p-5"
      onSubmit={(e) => {
        e.preventDefault();
        if (personId) add.mutate();
      }}
    >
      {add.isError && (
        <Alert>
          {errorMessage(intl, add.error, { 409: messages.changed, 404: messages.changed })}
        </Alert>
      )}
      {available.length === 0 ? (
        <p className="text-muted-foreground text-sm">
          <FormattedMessage {...messages.everyoneAdded} />
        </p>
      ) : (
        <div className="flex flex-wrap items-end gap-3">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            <Label htmlFor={id}>
              <FormattedMessage {...messages.personToAdd} />
            </Label>
            <select
              id={id}
              required
              value={personId}
              onChange={(e) => setPersonId(e.target.value)}
              className="h-10 min-w-0 rounded-md border bg-surface px-3 text-sm sm:h-9"
              disabled={add.isPending}
            >
              <option value="">{intl.formatMessage(messages.choosePerson)}</option>
              {available.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.displayName} (@{p.username})
                </option>
              ))}
            </select>
          </div>
          <Button
            type="submit"
            loading={add.isPending}
            disabled={!available.some((p) => p.id === personId)}
          >
            <Plus aria-hidden="true" />
            <FormattedMessage {...messages.addMember} />
          </Button>
        </div>
      )}
    </form>
  );
}

function TeamForm({
  team,
  onCancel,
  onSaved,
}: {
  team?: Schema<"Team">;
  onCancel: () => void;
  onSaved: (team: Schema<"Team">) => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const [name, setName] = useState(team?.name ?? "");
  const [slug, setSlug] = useState(team?.slug ?? "");
  const [description, setDescription] = useState(team?.description ?? "");
  const save = useMutation({
    mutationFn: () => {
      const body = { name: name.trim(), slug: slug.trim(), description: description.trim() };
      return team
        ? call(
            api.client.PATCH("/api/v1/teams/{teamId}", {
              params: { path: { teamId: team.id } },
              body,
            }),
          )
        : call(api.client.POST("/api/v1/teams", { body }));
    },
    onSuccess: async (saved) => {
      await queryClient.invalidateQueries({ queryKey: teamsKey });
      onSaved(saved);
    },
  });
  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate();
  }
  return (
    <Card className="mb-6 p-5">
      <h2 className="mb-4 font-semibold">
        <FormattedMessage {...(team ? messages.edit : messages.newTeam)} />
      </h2>
      <form onSubmit={submit} className="space-y-4">
        {save.isError && (
          <Alert>
            {errorMessage(intl, save.error, { 409: messages.conflict, 400: messages.invalid })}
          </Alert>
        )}
        <Field
          label={<FormattedMessage {...messages.name} />}
          value={name}
          onChange={(e) => setName(e.target.value)}
          maxLength={100}
          required
          disabled={save.isPending}
        />
        <Field
          label={<FormattedMessage {...messages.slug} />}
          hint={<FormattedMessage {...messages.slugHint} />}
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
          maxLength={63}
          pattern="[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?"
          required
          disabled={save.isPending}
        />
        <Field
          label={<FormattedMessage {...messages.teamDescription} />}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          maxLength={1000}
          disabled={save.isPending}
        />
        <div className="flex gap-2">
          <Button type="submit" loading={save.isPending}>
            <FormattedMessage {...(team ? messages.save : messages.create)} />
          </Button>
          <Button variant="ghost" disabled={save.isPending} onClick={onCancel}>
            <FormattedMessage {...messages.cancel} />
          </Button>
        </div>
      </form>
    </Card>
  );
}

export function TeamsPage() {
  const api = useApi();
  const intl = useIntl();
  const navigate = useNavigate();
  const manage = useMe().permissions.includes("people.manage");
  const [search, setSearch] = useState("");
  const [archived, setArchived] = useState(false);
  const [creating, setCreating] = useState(false);
  const teams = useQuery(teamsQuery(api, archived));
  const term = search.trim().toLocaleLowerCase(intl.locale);
  const shown =
    teams.data?.items.filter((t) =>
      `${t.name} ${t.slug} ${t.description ?? ""}`.toLocaleLowerCase(intl.locale).includes(term),
    ) ?? [];
  return (
    <Page
      title={intl.formatMessage(messages.title)}
      description={<FormattedMessage {...messages.description} />}
      actions={
        manage &&
        !creating && (
          <Button onClick={() => setCreating(true)}>
            <Plus aria-hidden="true" />
            <FormattedMessage {...messages.newTeam} />
          </Button>
        )
      }
    >
      {manage && creating && (
        <TeamForm
          onCancel={() => setCreating(false)}
          onSaved={(team) => void navigate({ to: "/teams/$teamId", params: { teamId: team.id } })}
        />
      )}
      <div className="mb-6 flex flex-wrap items-end gap-4">
        <Field
          className="min-w-0 flex-1"
          type="search"
          label={<FormattedMessage {...messages.search} />}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <label className="flex items-center gap-2 py-2 text-sm">
          <input
            type="checkbox"
            checked={archived}
            onChange={(e) => setArchived(e.target.checked)}
          />
          <FormattedMessage {...messages.showArchived} />
        </label>
      </div>
      {teams.isPending ? (
        <Loading />
      ) : teams.isError ? (
        <LoadError error={teams.error} retry={() => void teams.refetch()} />
      ) : shown.length === 0 ? (
        <EmptyState
          icon={UsersRound}
          title={<FormattedMessage {...(term ? messages.noMatches : messages.empty)} />}
          description={<FormattedMessage {...messages.emptyDescription} />}
        />
      ) : (
        <ul className="grid gap-4 sm:grid-cols-2">
          {shown.map((team) => (
            <li key={team.id}>
              <Link
                to="/teams/$teamId"
                params={{ teamId: team.id }}
                className="block h-full rounded-xl outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <Card className="flex h-full flex-col gap-3 p-5 transition-colors hover:border-input">
                  <div className="flex items-start justify-between gap-2">
                    <span className="font-semibold">{team.name}</span>
                    {team.archivedAt && (
                      <Badge>
                        <FormattedMessage {...messages.archived} />
                      </Badge>
                    )}
                  </div>
                  {team.description && (
                    <p className="break-words text-muted-foreground text-sm">{team.description}</p>
                  )}
                  <p className="mt-auto text-muted-foreground text-sm">
                    <FormattedMessage {...messages.count} values={{ count: team.memberCount }} />
                  </p>
                </Card>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Page>
  );
}

function Members({ teamId, manage }: { teamId: string; manage: boolean }) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const members = useQuery(teamMembersQuery(api, teamId));
  const change = useMutation({
    mutationFn: ({ personId, role }: { personId: string; role?: Schema<"TeamRole"> }) =>
      role
        ? call(
            api.client.PUT("/api/v1/teams/{teamId}/members/{personId}", {
              params: { path: { teamId, personId } },
              body: { role },
            }),
          )
        : call(
            api.client.DELETE("/api/v1/teams/{teamId}/members/{personId}", {
              params: { path: { teamId, personId } },
            }),
          ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: teamsKey }),
  });
  return (
    <Card className="overflow-hidden">
      <h2 className="border-b px-5 py-4 font-semibold">
        <FormattedMessage {...messages.members} />
      </h2>
      {change.isError && (
        <div className="p-5">
          <Alert>
            {errorMessage(intl, change.error, { 409: messages.changed, 404: messages.changed })}
          </Alert>
        </div>
      )}
      {members.isPending ? (
        <Loading />
      ) : members.isError ? (
        <LoadError error={members.error} retry={() => void members.refetch()} />
      ) : (
        <>
          {members.data.items.length === 0 ? (
            <p className="p-5 text-muted-foreground text-sm">
              <FormattedMessage {...messages.noMembers} />
            </p>
          ) : (
            <ul className="divide-y">
              {members.data.items.map(({ person, role }) => (
                <li key={person.id} className="flex flex-wrap items-center gap-3 p-5">
                  <Avatar name={person.displayName} seed={person.id} />
                  <div className="min-w-0 flex-1">
                    <p className="break-words font-medium text-sm">{person.displayName}</p>
                    <p className="text-muted-foreground text-sm">
                      {person.title ?? `@${person.username}`}
                    </p>
                  </div>
                  <Badge variant={role === "lead" ? "brand" : "neutral"}>
                    <FormattedMessage {...(role === "lead" ? messages.lead : messages.member)} />
                  </Badge>
                  {manage && (
                    <div className="flex w-full justify-end gap-1 sm:w-auto">
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={change.isPending}
                        aria-label={intl.formatMessage(
                          role === "lead" ? messages.makeMember : messages.makeLead,
                          { name: person.displayName },
                        )}
                        onClick={() =>
                          change.mutate({
                            personId: person.id,
                            role: role === "lead" ? "member" : "lead",
                          })
                        }
                      >
                        <FormattedMessage
                          {...(role === "lead" ? messages.makeMemberShort : messages.makeLeadShort)}
                        />
                      </Button>
                      <Button
                        variant="destructive-ghost"
                        size="sm"
                        disabled={change.isPending}
                        aria-label={intl.formatMessage(messages.remove, {
                          name: person.displayName,
                        })}
                        onClick={() => change.mutate({ personId: person.id })}
                      >
                        <FormattedMessage {...messages.removeShort} />
                      </Button>
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}
          {manage && <AddMember teamId={teamId} members={members.data.items} />}
        </>
      )}
    </Card>
  );
}

export function TeamPage({ teamId }: { teamId: string }) {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const manage = useMe().permissions.includes("people.manage");
  const team = useQuery(teamQuery(api, teamId));
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const archive = useMutation({
    mutationFn: () =>
      call(api.client.POST("/api/v1/teams/{teamId}/archive", { params: { path: { teamId } } })),
    onSuccess: async () => {
      setConfirming(false);
      await queryClient.invalidateQueries({ queryKey: teamsKey });
    },
  });
  const active = manage && !team.data?.archivedAt;
  return (
    <Page
      title={team.data?.name ?? intl.formatMessage(messages.title)}
      description={team.data?.description}
      actions={
        team.data &&
        active &&
        !editing && (
          <Button variant="outline" onClick={() => setEditing(true)}>
            <FormattedMessage {...messages.edit} />
          </Button>
        )
      }
    >
      <Link to="/teams" className="mb-6 inline-block text-primary text-sm hover:underline">
        <FormattedMessage {...messages.back} />
      </Link>
      {team.isPending ? (
        <Loading />
      ) : team.isError ? (
        hasStatus(team.error, 404) ? (
          <Alert>{intl.formatMessage(messages.notFound)}</Alert>
        ) : (
          <LoadError error={team.error} retry={() => void team.refetch()} />
        )
      ) : (
        <>
          {team.data.archivedAt && (
            <div className="mb-6">
              <Alert variant="info">
                <FormattedMessage {...messages.archivedInfo} />
              </Alert>
            </div>
          )}
          {editing && active && (
            <TeamForm
              key={teamId}
              team={team.data}
              onCancel={() => setEditing(false)}
              onSaved={() => setEditing(false)}
            />
          )}
          <Members key={teamId} teamId={teamId} manage={active} />
          {active && (
            <div className="mt-6 space-y-3">
              {archive.isError && (
                <Alert>
                  {errorMessage(intl, archive.error, { 409: messages.archiveConflict })}
                </Alert>
              )}
              {confirming ? (
                <Card className="space-y-4 p-5">
                  <p className="text-sm">
                    <FormattedMessage {...messages.archiveQuestion} />
                  </p>
                  <div className="flex gap-2">
                    <Button
                      variant="destructive"
                      loading={archive.isPending}
                      onClick={() => archive.mutate()}
                    >
                      <FormattedMessage {...messages.confirmArchive} />
                    </Button>
                    <Button
                      variant="ghost"
                      disabled={archive.isPending}
                      onClick={() => setConfirming(false)}
                    >
                      <FormattedMessage {...messages.cancel} />
                    </Button>
                  </div>
                </Card>
              ) : (
                <Button variant="destructive-ghost" onClick={() => setConfirming(true)}>
                  <FormattedMessage {...messages.archive} />
                </Button>
              )}
            </div>
          )}
        </>
      )}
    </Page>
  );
}
