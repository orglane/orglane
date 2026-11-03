import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { FormattedMessage, useIntl } from "react-intl";
import { Choice } from "@/components/choice";
import { Field } from "@/components/field";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { onboardingKey } from "@/lib/onboarding";
import {
  displayValue,
  fieldLabel,
  fieldMessages as m,
  profileKey,
  sectionMessages,
} from "@/lib/profile-fields";
import { useMe } from "@/lib/session";

function ProfileValue({
  entry,
  personId,
}: {
  entry: Schema<"ProfileFieldValue">;
  personId: string;
}) {
  const intl = useIntl();
  const api = useApi();
  const client = useQueryClient();
  const self = useMe().person.id === personId;
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  const [pendingHidden, setPendingHidden] = useState<boolean | undefined>(undefined);
  const field = entry.field;
  const label = fieldLabel(intl, field);
  const change = useMutation({
    mutationFn: (value: Schema<"ProfileFieldValueChange">) =>
      call(
        api.client.PATCH("/api/v1/people/{personId}/profile", {
          params: { path: { personId } },
          body: { values: [value] },
        }),
      ),
    onSuccess: (result) => {
      client.setQueryData(profileKey(personId), result);
      void client.invalidateQueries({ queryKey: onboardingKey });
      setEditing(false);
      setPendingHidden(undefined);
    },
    onError: () => setPendingHidden(undefined),
  });
  return (
    <li className="space-y-3 py-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="font-medium text-sm">{label}</h3>
          <p
            className="mt-1 whitespace-pre-wrap break-words text-sm"
            data-testid={`profile-value-${field.key}`}
          >
            {displayValue(intl, entry) || "—"}
          </p>
          {entry.hidden && (
            <p className="mt-1 text-muted-foreground text-xs">
              <FormattedMessage {...m.hidden} />
            </p>
          )}
        </div>
        {entry.editable && !editing && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              change.reset();
              setValue(entry.yearHidden ? "" : (entry.value ?? ""));
              setEditing(true);
            }}
          >
            <FormattedMessage {...m.edit} />
          </Button>
        )}
      </div>
      {field.library && field.key === "birthday" && (
        <p className="text-muted-foreground text-xs">
          <FormattedMessage {...m.yearHint} />
        </p>
      )}
      {editing && (
        <form
          className="max-w-lg space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            change.mutate({ key: field.key, value });
          }}
        >
          {field.type === "select" ? (
            <Choice label={label} value={value} onChange={setValue}>
              <option value="">{"—"}</option>
              {field.options.map((o) => (
                <option key={o.key} value={o.key}>
                  {o.label}
                </option>
              ))}
            </Choice>
          ) : field.type === "long_text" ? (
            <label className="block space-y-2 text-sm">
              <span>{label}</span>
              <textarea
                className="w-full rounded-md border bg-background p-3"
                value={value}
                onChange={(e) => setValue(e.target.value)}
                rows={4}
                maxLength={2000}
              />
            </label>
          ) : (
            <Field
              label={label}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              type={
                field.type === "date"
                  ? "date"
                  : field.type === "url"
                    ? "url"
                    : field.type === "phone"
                      ? "tel"
                      : "text"
              }
              required={entry.yearHidden && !!entry.value}
              maxLength={field.type === "text" ? 200 : field.type === "url" ? 2000 : undefined}
              autoFocus
            />
          )}
          <div className="flex gap-2">
            <Button type="submit" loading={change.isPending}>
              <FormattedMessage {...m.save} />
            </Button>
            <Button
              type="button"
              variant="ghost"
              disabled={change.isPending}
              onClick={() => {
                setEditing(false);
                change.reset();
              }}
            >
              <FormattedMessage {...m.cancel} />
            </Button>
            {entry.value && (
              <Button
                type="button"
                variant="ghost"
                disabled={change.isPending}
                onClick={() => change.mutate({ key: field.key, value: "" })}
              >
                <FormattedMessage {...m.clear} />
              </Button>
            )}
          </div>
        </form>
      )}
      {self && (entry.hideable || entry.hidden) && entry.value && (
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="size-4 accent-primary"
            checked={pendingHidden ?? entry.hidden}
            disabled={change.isPending}
            onChange={(e) => {
              setPendingHidden(e.target.checked);
              change.mutate({ key: field.key, hidden: e.target.checked });
            }}
          />
          <FormattedMessage {...m.hide} />
        </label>
      )}
      {change.error && <Alert>{errorMessage(intl, change.error, { 400: m.invalid })}</Alert>}
    </li>
  );
}

/** Every entry is already authorized by the profile service. */
export function ProfileFieldsCard({
  personId,
  requiredOnly = false,
}: {
  personId: string;
  requiredOnly?: boolean;
}) {
  const intl = useIntl();
  const api = useApi();
  const profile = useQuery({
    queryKey: profileKey(personId),
    queryFn: () =>
      call(api.client.GET("/api/v1/people/{personId}/profile", { params: { path: { personId } } })),
    retry: false,
  });
  const fields =
    profile.data?.fields.filter(
      (e) => !requiredOnly || (e.field.requiredAtOnboarding && e.editable),
    ) ?? [];
  return (
    <Card className="p-5 sm:p-6">
      <h2 className="mb-4 font-semibold text-lg">
        <FormattedMessage {...m.title} />
      </h2>
      {profile.isPending && (
        <p role="status">
          <FormattedMessage {...m.loading} />
        </p>
      )}
      {profile.error && (
        <div className="space-y-3">
          <Alert>{errorMessage(intl, profile.error, { 404: m.notFound })}</Alert>
          <Button
            onClick={() => {
              void profile.refetch();
            }}
          >
            <FormattedMessage {...m.retry} />
          </Button>
        </div>
      )}
      {profile.data && fields.length === 0 && (
        <p className="text-muted-foreground text-sm">
          <FormattedMessage {...m.empty} />
        </p>
      )}
      {profile.data &&
        [...new Set(fields.map((e) => e.field.section))].map((section) => (
          <section key={section} className="mb-4">
            <h2 className="border-b pb-2 font-medium text-muted-foreground text-sm">
              {intl.formatMessage(sectionMessages[section])}
            </h2>
            <ul className="divide-y">
              {fields
                .filter((e) => e.field.section === section)
                .map((entry) => (
                  <ProfileValue key={entry.field.id} entry={entry} personId={personId} />
                ))}
            </ul>
          </section>
        ))}
    </Card>
  );
}
