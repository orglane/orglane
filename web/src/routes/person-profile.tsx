import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { FormattedMessage, useIntl } from "react-intl";

import { Choice } from "@/components/choice";
import { Field } from "@/components/field";
import { OnboardingChecklist } from "@/components/onboarding-checklist";
import { Page } from "@/components/page";
import { ProfileFieldsCard } from "@/components/profile-fields-card";
import { Alert } from "@/components/ui/alert";
import { Avatar } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ApiError, call, meKey, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { onboardingKey } from "@/lib/onboarding";
import { peopleMessages as m, peopleKey, personKey, personQuery } from "@/lib/people";
import { useMe } from "@/lib/session";

function BaseProfileForm({
  person,
  manager,
  manage,
  onSaved,
  onCancel,
}: {
  person: Schema<"Person">;
  manager?: Schema<"Person"> | undefined;
  manage: boolean;
  onSaved: () => void;
  onCancel: () => void;
}) {
  const api = useApi();
  const intl = useIntl();
  const client = useQueryClient();
  const [displayName, setDisplayName] = useState(person.displayName);
  const [title, setTitle] = useState(person.title ?? "");
  const [username, setUsername] = useState(person.username);
  const [workEmail, setWorkEmail] = useState(person.workEmail);
  const [managerId, setManagerId] = useState(person.managerId ?? "");
  const [managerName, setManagerName] = useState(manager?.displayName ?? person.managerId ?? "");
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  useEffect(() => {
    const timer = window.setTimeout(() => setQuery(search.trim()), 250);
    return () => window.clearTimeout(timer);
  }, [search]);
  const candidates = useQuery({
    queryKey: [...peopleKey, "manager-options", query],
    queryFn: () =>
      call(api.client.GET("/api/v1/people", { params: { query: { q: query, limit: 50 } } })),
    enabled: manage,
  });
  const choices = candidates.data?.items.filter((p) => p.id !== person.id) ?? [];
  const save = useMutation({
    mutationFn: () =>
      call(
        api.client.PATCH("/api/v1/people/{personId}", {
          params: { path: { personId: person.id } },
          body: { displayName, title, ...(manage ? { username, workEmail, managerId } : {}) },
        }),
      ),
    onSuccess: async (saved) => {
      client.setQueryData(personKey(person.id), saved);
      await client.invalidateQueries({ queryKey: peopleKey });
      await client.invalidateQueries({ queryKey: meKey });
      await client.invalidateQueries({ queryKey: onboardingKey });
      onSaved();
    },
  });
  return (
    <Card className="mb-5 p-5 sm:p-6">
      <h2 className="mb-4 font-semibold">
        <FormattedMessage {...m.edit} />
      </h2>
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <fieldset className="space-y-4" disabled={save.isPending}>
          <Field
            label={intl.formatMessage(m.name)}
            value={displayName}
            required
            maxLength={200}
            onChange={(e) => setDisplayName(e.target.value)}
          />
          <Field
            label={intl.formatMessage(m.jobTitle)}
            value={title}
            maxLength={200}
            onChange={(e) => setTitle(e.target.value)}
          />
          {manage && (
            <>
              <Field
                label={intl.formatMessage(m.username)}
                value={username}
                required
                maxLength={63}
                onChange={(e) => setUsername(e.target.value)}
              />
              <Field
                label={intl.formatMessage(m.email)}
                value={workEmail}
                type="email"
                required
                maxLength={254}
                onChange={(e) => setWorkEmail(e.target.value)}
              />
              <Field
                label={intl.formatMessage(m.managerSearch)}
                hint={intl.formatMessage(m.managerHint)}
                value={search}
                type="search"
                onChange={(e) => setSearch(e.target.value)}
              />
              {candidates.isPending && (
                <p role="status">
                  <FormattedMessage {...m.loading} />
                </p>
              )}
              {candidates.error && (
                <Alert>
                  {errorMessage(intl, candidates.error)}
                  <Button variant="link" onClick={() => void candidates.refetch()}>
                    <FormattedMessage {...m.retry} />
                  </Button>
                </Alert>
              )}
              <Choice
                label={intl.formatMessage(m.manager)}
                value={managerId}
                onChange={(id) => {
                  setManagerId(id);
                  setManagerName(choices.find((p) => p.id === id)?.displayName ?? "");
                }}
              >
                <option value="">{intl.formatMessage(m.noManager)}</option>
                {managerId && !choices.some((p) => p.id === managerId) && (
                  <option value={managerId}>{managerName}</option>
                )}
                {choices.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.displayName} (@{p.username})
                  </option>
                ))}
              </Choice>
            </>
          )}
        </fieldset>
        {save.error && (
          <Alert>
            {errorMessage(intl, save.error, { 400: m.invalid, 409: m.conflict, 404: m.notFound })}
          </Alert>
        )}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" loading={save.isPending}>
            <FormattedMessage {...m.save} />
          </Button>
          <Button variant="ghost" disabled={save.isPending} onClick={onCancel}>
            <FormattedMessage {...m.cancel} />
          </Button>
        </div>
      </form>
    </Card>
  );
}

export function PersonProfilePage({ personId }: { personId: string }) {
  const intl = useIntl();
  const api = useApi();
  const client = useQueryClient();
  const me = useMe();
  const person = useQuery(personQuery(api, personId));
  const manager = useQuery({
    ...personQuery(api, person.data?.managerId ?? ""),
    enabled: !!person.data?.managerId,
  });
  const manage = me.permissions.includes("people.manage");
  const canEdit = me.person.id === personId || manage;
  const [editing, setEditing] = useState(false);
  const [saved, setSaved] = useState(false);
  const upload = useMutation({
    mutationFn: async (file: File) => {
      if (
        file.size > 5 * 1024 * 1024 ||
        !["image/png", "image/jpeg", "image/webp"].includes(file.type)
      ) {
        throw new ApiError({ type: "about:blank", status: 400, title: "Invalid avatar" });
      }
      // OpenAPI's binary schema is typed as string; the serializer passes the
      // original Blob bytes, with the concrete raster MIME type, to fetch.
      return call(
        api.client.PUT("/api/v1/people/{personId}/avatar", {
          params: { path: { personId } },
          headers: { "Content-Type": file.type },
          body: file as unknown as string,
          bodySerializer: () => file,
        }),
      );
    },
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: peopleKey });
      await client.invalidateQueries({ queryKey: onboardingKey });
    },
  });
  const p = person.data;
  return (
    <Page
      title={p?.displayName ?? intl.formatMessage(m.profile)}
      description={p?.title}
      actions={
        p && canEdit && !editing ? (
          <Button
            variant="outline"
            onClick={() => {
              setEditing(true);
              setSaved(false);
            }}
          >
            <FormattedMessage {...m.edit} />
          </Button>
        ) : undefined
      }
    >
      <Link to="/people" className="mb-5 inline-block text-primary text-sm hover:underline">
        <FormattedMessage {...m.back} />
      </Link>
      {person.isPending && (
        <p role="status">
          <FormattedMessage {...m.loading} />
        </p>
      )}
      {person.error && (
        <Alert>
          {errorMessage(intl, person.error, { 404: m.notFound })}
          <Button variant="link" onClick={() => void person.refetch()}>
            <FormattedMessage {...m.retry} />
          </Button>
        </Alert>
      )}
      {p && (
        <>
          <Card className="mb-5 space-y-5 p-5 sm:p-6">
            <div className="flex items-start gap-4">
              <Avatar
                name={p.displayName}
                src={p.avatarUrl}
                className="size-16 text-xl sm:size-20"
              />
              <div className="min-w-0 space-y-2">
                <p className="break-all text-muted-foreground text-sm">@{p.username}</p>
                <Badge variant="neutral">{intl.formatMessage(m[p.status])}</Badge>
                <div className="flex flex-wrap gap-2">
                  {p.teams.map((t) => (
                    <Link
                      key={t.id}
                      to="/teams/$teamId"
                      params={{ teamId: t.id }}
                      className="rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    >
                      <Badge variant="neutral">{t.name}</Badge>
                    </Link>
                  ))}
                </div>
              </div>
            </div>
            <dl className="grid gap-4 text-sm sm:grid-cols-2">
              <div className="min-w-0">
                <dt className="text-muted-foreground">
                  <FormattedMessage {...m.contact} />
                </dt>
                <dd className="mt-1 break-all">
                  <a className="text-primary hover:underline" href={`mailto:${p.workEmail}`}>
                    {p.workEmail}
                  </a>
                </dd>
              </div>
              <div>
                <dt className="text-muted-foreground">
                  <FormattedMessage {...m.manager} />
                </dt>
                <dd className="mt-1">
                  {manager.data ? (
                    <Link
                      to="/people/$personId"
                      params={{ personId: manager.data.id }}
                      className="text-primary hover:underline"
                    >
                      {manager.data.displayName}
                    </Link>
                  ) : manager.isFetching && p.managerId ? (
                    <FormattedMessage {...m.loading} />
                  ) : p.managerId ? (
                    <Alert>
                      <FormattedMessage {...m.managerUnavailable} />
                      <Button variant="link" onClick={() => void manager.refetch()}>
                        <FormattedMessage {...m.retry} />
                      </Button>
                    </Alert>
                  ) : (
                    <FormattedMessage {...m.noManager} />
                  )}
                </dd>
              </div>
            </dl>
            {canEdit && (
              <div className="max-w-lg space-y-3 border-t pt-4">
                <Field
                  label={intl.formatMessage(m.avatar)}
                  hint={intl.formatMessage(m.avatarHint)}
                  type="file"
                  accept="image/png,image/jpeg,image/webp"
                  disabled={upload.isPending}
                  onChange={(e) => {
                    const file = e.target.files?.[0];
                    if (file) {
                      upload.mutate(file);
                      e.target.value = "";
                    }
                  }}
                />
                {upload.isPending && (
                  <p role="status">
                    <FormattedMessage {...m.loading} />
                  </p>
                )}
                {upload.error && (
                  <Alert>
                    {errorMessage(intl, upload.error, {
                      400: m.avatarInvalid,
                      413: m.avatarInvalid,
                      415: m.avatarInvalid,
                    })}
                  </Alert>
                )}
                {upload.isSuccess && (
                  <Alert variant="success">
                    <FormattedMessage {...m.avatarSaved} />
                  </Alert>
                )}
              </div>
            )}
          </Card>
          {editing && canEdit && (
            <BaseProfileForm
              person={p}
              manager={manager.data}
              manage={manage}
              onCancel={() => setEditing(false)}
              onSaved={() => {
                setEditing(false);
                setSaved(true);
              }}
            />
          )}
          {saved && (
            <Alert variant="success" className="mb-5">
              <FormattedMessage {...m.saved} />
            </Alert>
          )}
          <ProfileFieldsCard personId={personId} />
          {manage && (
            <div className="mt-5">
              <OnboardingChecklist personId={personId} />
            </div>
          )}
        </>
      )}
    </Page>
  );
}
