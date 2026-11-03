import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { FormattedMessage, useIntl } from "react-intl";
import { Choice, selectClass } from "@/components/choice";
import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import {
  fieldLabel,
  fieldsKey,
  fieldTypes,
  handleMessages,
  libraryKey,
  fieldMessages as m,
  type ProfileField,
  sectionMessages,
  sections,
  typeMessages,
  visibilities,
} from "@/lib/profile-fields";
import { useMe } from "@/lib/session";

function Check({
  label,
  value,
  onChange,
  disabled = false,
}: {
  label: ReactNode;
  value: boolean;
  onChange: (value: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className="flex items-center gap-2 text-sm">
      <input
        type="checkbox"
        className="size-4 accent-primary"
        checked={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      {label}
    </label>
  );
}

type Settings = Pick<
  ProfileField,
  "visibility" | "section" | "editableBy" | "requiredAtOnboarding" | "userCanHide" | "sensitive"
>;
const defaults: Settings = {
  visibility: "everyone",
  section: "custom",
  editableBy: "self",
  requiredAtOnboarding: false,
  userCanHide: false,
  sensitive: false,
};

function SettingsInputs({
  value,
  onChange,
  protectSensitive = false,
}: {
  value: Settings;
  onChange: (value: Settings) => void;
  protectSensitive?: boolean;
}) {
  const intl = useIntl();
  return (
    <div className="space-y-4">
      <Choice
        label={intl.formatMessage(m.visibility)}
        value={value.visibility ?? "everyone"}
        onChange={(v) => onChange({ ...value, visibility: v as Settings["visibility"] })}
      >
        {visibilities.map((v) => (
          <option key={v} value={v}>
            {intl.formatMessage(m[v])}
          </option>
        ))}
      </Choice>
      <details className="rounded-lg border p-4">
        <summary className="cursor-pointer font-medium text-sm">
          <FormattedMessage {...m.advanced} />
        </summary>
        <div className="mt-4 space-y-4">
          <Choice
            label={intl.formatMessage(m.section)}
            value={value.section ?? "custom"}
            onChange={(v) => onChange({ ...value, section: v as Settings["section"] })}
          >
            {sections.map((v) => (
              <option key={v} value={v}>
                {intl.formatMessage(sectionMessages[v])}
              </option>
            ))}
          </Choice>
          <Choice
            label={intl.formatMessage(m.editor)}
            value={value.editableBy ?? "self"}
            onChange={(v) => onChange({ ...value, editableBy: v as Settings["editableBy"] })}
          >
            <option value="self">{intl.formatMessage(m.self)}</option>
            <option value="admins">{intl.formatMessage(m.adminsEditor)}</option>
          </Choice>
          <Check
            label={intl.formatMessage(m.required)}
            value={value.requiredAtOnboarding ?? false}
            onChange={(v) => onChange({ ...value, requiredAtOnboarding: v })}
          />
          <Check
            label={intl.formatMessage(m.hideable)}
            value={value.userCanHide ?? false}
            onChange={(v) => onChange({ ...value, userCanHide: v })}
          />
          <Check
            label={intl.formatMessage(m.sensitive)}
            value={value.sensitive ?? false}
            disabled={protectSensitive}
            onChange={(v) => onChange({ ...value, sensitive: v })}
          />
          <p className="text-muted-foreground text-xs">
            <FormattedMessage {...m.sensitiveHint} />
          </p>
        </div>
      </details>
    </div>
  );
}

function Failure({ error }: { error: unknown }) {
  const intl = useIntl();
  return <Alert>{errorMessage(intl, error, { 400: m.invalid, 409: m.conflict })}</Alert>;
}

function CreateField({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const intl = useIntl();
  const api = useApi();
  const [step, setStep] = useState(0);
  const [label, setLabel] = useState("");
  const [type, setType] = useState<Schema<"ProfileFieldType">>("text");
  const [handleKind, setHandleKind] = useState<Schema<"ProfileFieldHandleKind">>("telegram");
  const [options, setOptions] = useState("");
  const [settings, setSettings] = useState(defaults);
  const mutation = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/profile-fields", {
          body: {
            label,
            type,
            ...settings,
            ...(type === "handle" ? { handleKind } : {}),
            ...(type === "select"
              ? {
                  options: options
                    .split("\n")
                    .map((label) => ({ label: label.trim() }))
                    .filter((o) => o.label),
                }
              : {}),
          },
        }),
      ),
    onSuccess: onCreated,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (step < 2) setStep(step + 1);
    else mutation.mutate();
  };
  return (
    <form
      onSubmit={submit}
      className="space-y-5 rounded-lg border p-5"
      aria-label={intl.formatMessage(m.add)}
    >
      <h3 className="font-medium">
        {intl.formatMessage([m.stepName, m.stepType, m.stepVisibility][step] ?? m.stepName)}
      </h3>
      {step === 0 && (
        <Field
          label={intl.formatMessage(m.name)}
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          required
          maxLength={100}
          autoFocus
        />
      )}
      {step === 1 && (
        <>
          <Choice
            label={intl.formatMessage(m.type)}
            value={type}
            onChange={(v) => setType(v as typeof type)}
          >
            {fieldTypes.map((t) => (
              <option key={t} value={t}>
                {intl.formatMessage(typeMessages[t])}
              </option>
            ))}
          </Choice>
          {type === "handle" && (
            <Choice
              label={intl.formatMessage(m.handleKind)}
              value={handleKind}
              onChange={(v) => setHandleKind(v as typeof handleKind)}
            >
              {Object.entries(handleMessages).map(([key, message]) => (
                <option key={key} value={key}>
                  {intl.formatMessage(message)}
                </option>
              ))}
            </Choice>
          )}
          {type === "select" && (
            <label className="block space-y-2 text-sm">
              <span>{intl.formatMessage(m.options)}</span>
              <textarea
                className={selectClass}
                required
                rows={4}
                value={options}
                onChange={(e) => setOptions(e.target.value)}
              />
            </label>
          )}
        </>
      )}
      {step === 2 && <SettingsInputs value={settings} onChange={setSettings} />}
      {mutation.error && <Failure error={mutation.error} />}
      <div className="flex flex-wrap gap-2">
        <Button type="button" variant="ghost" onClick={onClose} disabled={mutation.isPending}>
          <FormattedMessage {...m.cancel} />
        </Button>
        {step > 0 && (
          <Button
            type="button"
            variant="outline"
            onClick={() => setStep(step - 1)}
            disabled={mutation.isPending}
          >
            <FormattedMessage {...m.back} />
          </Button>
        )}
        <Button type="submit" loading={mutation.isPending}>
          <FormattedMessage {...(step < 2 ? m.next : m.create)} />
        </Button>
      </div>
    </form>
  );
}

function FieldSettings({ field, onSaved }: { field: ProfileField; onSaved: () => void }) {
  const intl = useIntl();
  const api = useApi();
  const me = useMe();
  const [settings, setSettings] = useState<Settings>({
    visibility: field.visibility,
    section: field.section,
    editableBy: field.editableBy,
    requiredAtOnboarding: field.requiredAtOnboarding,
    userCanHide: field.userCanHide,
    sensitive: field.sensitive,
  });
  const [label, setLabel] = useState(field.label);
  const [position, setPosition] = useState(field.position);
  const mutation = useMutation({
    mutationFn: () =>
      call(
        api.client.PATCH("/api/v1/profile-fields/{fieldId}", {
          params: { path: { fieldId: field.id } },
          body: { ...settings, ...(!field.library ? { label } : {}), position },
        }),
      ),
    onSuccess: onSaved,
  });
  return (
    <details className="mt-3">
      <summary className="cursor-pointer text-sm text-muted-foreground">
        <FormattedMessage {...m.settings} />
      </summary>
      <form
        className="mt-4 max-w-lg space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          mutation.mutate();
        }}
      >
        {!field.library && (
          <Field
            label={intl.formatMessage(m.name)}
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            required
            maxLength={100}
          />
        )}
        <SettingsInputs
          value={settings}
          onChange={setSettings}
          protectSensitive={field.sensitive && !me.permissions.includes("people.sensitive.read")}
        />
        <Field
          label={intl.formatMessage(m.order)}
          type="number"
          min={0}
          max={1000000}
          value={position}
          onChange={(e) => setPosition(Number(e.target.value))}
          required
        />
        {mutation.error && <Failure error={mutation.error} />}
        {mutation.isSuccess && (
          <Alert variant="success">
            <FormattedMessage {...m.saved} />
          </Alert>
        )}
        <Button type="submit" loading={mutation.isPending}>
          <FormattedMessage {...m.save} />
        </Button>
      </form>
    </details>
  );
}

export function ProfileFieldsPage() {
  const intl = useIntl();
  const me = useMe();
  const api = useApi();
  const queryClient = useQueryClient();
  const allowed = me.permissions.includes("people.manage");
  const [creating, setCreating] = useState(false);
  const library = useQuery({
    queryKey: libraryKey,
    queryFn: () => call(api.client.GET("/api/v1/profile-field-library")),
    enabled: allowed,
  });
  const fields = useQuery({
    queryKey: fieldsKey,
    queryFn: () =>
      call(
        api.client.GET("/api/v1/profile-fields", { params: { query: { includeArchived: true } } }),
      ),
    enabled: allowed,
  });
  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: fieldsKey }),
      queryClient.invalidateQueries({ queryKey: libraryKey }),
      queryClient.invalidateQueries({ queryKey: ["person-profile"] }),
    ]);
  };
  const toggle = useMutation({
    mutationFn: (item: { key: string; field?: ProfileField }) =>
      item.field && !item.field.archived
        ? call(
            api.client.PATCH("/api/v1/profile-fields/{fieldId}", {
              params: { path: { fieldId: item.field.id } },
              body: { archived: true },
            }),
          )
        : call(
            api.client.POST("/api/v1/profile-field-library/{libraryKey}", {
              params: { path: { libraryKey: item.key } },
            }),
          ),
    onSuccess: refresh,
  });
  const archive = useMutation({
    mutationFn: (field: ProfileField) =>
      call(
        api.client.PATCH("/api/v1/profile-fields/{fieldId}", {
          params: { path: { fieldId: field.id } },
          body: { archived: !field.archived },
        }),
      ),
    onSuccess: refresh,
  });
  return (
    <Page title={intl.formatMessage(m.title)} description={intl.formatMessage(m.description)}>
      {!allowed ? (
        <Alert>
          <FormattedMessage {...m.forbidden} />
        </Alert>
      ) : (
        <div className="space-y-6">
          {(library.error || fields.error) && (
            <>
              <Failure error={library.error ?? fields.error} />
              <Button
                onClick={() => {
                  void library.refetch();
                  void fields.refetch();
                }}
              >
                <FormattedMessage {...m.retry} />
              </Button>
            </>
          )}
          {(library.isPending || fields.isPending) && (
            <p role="status">
              <FormattedMessage {...m.loading} />
            </p>
          )}
          {(toggle.error || archive.error) && <Failure error={toggle.error ?? archive.error} />}
          <Card className="p-5 sm:p-6">
            <h2 className="mb-4 font-semibold text-lg">
              <FormattedMessage {...m.library} />
            </h2>
            <ul className="divide-y">
              {library.data?.items.map((item) => {
                const field = fields.data?.items.find((f) => f.key === item.key) ?? item.field;
                const enabled = field && !field.archived;
                const label = fieldLabel(intl, item);
                return (
                  <li key={item.key} className="py-4">
                    <div className="flex items-center justify-between gap-4">
                      <div>
                        <h3 className="font-medium">{label}</h3>
                        <p className="text-muted-foreground text-sm">
                          {intl.formatMessage(typeMessages[item.type])}
                          {" · "}
                          {intl.formatMessage(m[field?.visibility ?? item.visibility])}
                        </p>
                      </div>
                      <Button
                        role="switch"
                        aria-checked={!!enabled}
                        aria-label={label}
                        variant={enabled ? "outline" : "primary"}
                        disabled={toggle.isPending}
                        onClick={() =>
                          toggle.mutate({ key: item.key, ...(field ? { field } : {}) })
                        }
                      >
                        <FormattedMessage {...(enabled ? m.disable : m.enable)} />
                      </Button>
                    </div>
                    {enabled && (
                      <FieldSettings
                        key={`${field.id}-${field.visibility}-${field.sensitive}-${field.position}`}
                        field={field}
                        onSaved={() => {
                          void refresh();
                        }}
                      />
                    )}
                  </li>
                );
              })}
            </ul>
          </Card>
          <Card className="space-y-5 p-5 sm:p-6">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h2 className="font-semibold text-lg">
                <FormattedMessage {...m.custom} />
              </h2>
              <Button onClick={() => setCreating(true)} disabled={creating}>
                <FormattedMessage {...m.add} />
              </Button>
            </div>
            {creating && (
              <CreateField
                onClose={() => setCreating(false)}
                onCreated={() => {
                  setCreating(false);
                  void refresh();
                }}
              />
            )}
            <ul className="divide-y">
              {fields.data?.items
                .filter((f) => !f.library)
                .map((field) => (
                  <li key={field.id} className="py-4">
                    <div className="flex items-center justify-between gap-4">
                      <div>
                        <h3 className="font-medium">{field.label}</h3>
                        <p className="text-muted-foreground text-sm">
                          {intl.formatMessage(typeMessages[field.type])}
                          {" · "}
                          {intl.formatMessage(m[field.visibility])}
                        </p>
                      </div>
                      <Button
                        variant="outline"
                        disabled={archive.isPending}
                        onClick={() => archive.mutate(field)}
                      >
                        <FormattedMessage {...(field.archived ? m.restore : m.archive)} />
                      </Button>
                    </div>
                    {!field.archived && (
                      <FieldSettings
                        field={field}
                        onSaved={() => {
                          void refresh();
                        }}
                      />
                    )}
                  </li>
                ))}
            </ul>
          </Card>
        </div>
      )}
    </Page>
  );
}
