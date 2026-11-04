import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ApiError, call, hasStatus, NetworkError, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";
import { useEndSession } from "@/lib/session-guard";

const messages = defineMessages({
  githubPending: {
    id: "import.githubPending",
    defaultMessage: "Fetching members. This can take up to five minutes.",
  },
  githubFailure: {
    id: "import.githubFailure",
    defaultMessage:
      "GitHub fetch failed. Check organization membership, token access, rate limits and the 2,000-member limit, then retry. No partial run was saved.",
  },
  source: { id: "import.source", defaultMessage: "Import source" },
  spreadsheet: { id: "import.spreadsheet", defaultMessage: "Spreadsheet" },
  github: { id: "import.github", defaultMessage: "GitHub organization" },
  organization: { id: "import.organization", defaultMessage: "Organization name" },
  token: { id: "import.token", defaultMessage: "Short-lived fine-grained token" },
  fetch: { id: "import.fetch", defaultMessage: "Fetch members" },
  githubHint: {
    id: "import.githubHint",
    defaultMessage:
      "Up to 2,000 members. Use a short-lived token with organization Members read access. Failed or incomplete fetches create no run. The token is discarded after fetching. Public email is optional; new people still need a work email and full name. Existing people keep all local fields and grants; only their pending account association is updated. Account associations remain unverified. Avatars are recorded as source evidence only.",
  },

  title: { id: "import.title", defaultMessage: "Import people" },
  description: {
    id: "import.description",
    defaultMessage:
      "Upload a spreadsheet, map columns, and review every row before applying changes.",
  },
  file: { id: "import.file", defaultMessage: "CSV or XLSX file" },
  limits: {
    id: "import.limits",
    defaultMessage:
      "Up to 5 MiB and 1,000 rows. UTF-8, UTF-16 and Windows-1252 CSV; first worksheet of XLSX. Formula rows are rejected.",
  },
  template: { id: "import.template", defaultMessage: "Download CSV template" },
  upload: { id: "import.upload", defaultMessage: "Upload file" },
  mapping: { id: "import.mapping", defaultMessage: "Map columns" },
  createHint: {
    id: "import.createHint",
    defaultMessage:
      "New people require a username, work email and full name. The default role is Member; contact email is required for email delivery.",
  },
  mappingHint: {
    id: "import.mappingHint",
    defaultMessage:
      "Blank cells preserve existing data. Teams use semicolons; roles add a permission bundle. Import managers before their reports. Unknown headers are numbered to keep sensitive data private.",
  },
  ignore: { id: "import.ignore", defaultMessage: "Ignore this column" },
  username: { id: "import.target.username", defaultMessage: "Username" },
  work_email: { id: "import.target.workEmail", defaultMessage: "Work email" },
  contact_email: { id: "import.target.contactEmail", defaultMessage: "Contact email" },
  display_name: { id: "import.target.displayName", defaultMessage: "Full name" },
  titleField: { id: "import.target.title", defaultMessage: "Job title" },
  teams: { id: "import.target.teams", defaultMessage: "Teams by name" },
  role: { id: "import.target.role", defaultMessage: "Role" },
  manager_email: { id: "import.target.managerEmail", defaultMessage: "Manager work email" },
  external_id: { id: "import.target.externalId", defaultMessage: "External ID" },
  createTeams: { id: "import.createTeams", defaultMessage: "Create teams that do not exist" },
  sendInvitations: {
    id: "import.sendInvitations",
    defaultMessage: "Email invitations for newly created people",
  },
  invitationHint: {
    id: "import.invitationHint",
    defaultMessage:
      "Requires a trusted email channel for the requested grant. Otherwise leave this off and use Invitations to issue links after import.",
  },
  preview: { id: "import.preview", defaultMessage: "Preview changes" },
  previewTitle: { id: "import.previewTitle", defaultMessage: "Review changes" },
  privacy: {
    id: "import.privacy",
    defaultMessage:
      "Source values stay private. Use row numbers in your original file to correct errors. Skipped rows make no changes.",
  },
  apply: { id: "import.apply", defaultMessage: "Apply import" },
  editMapping: { id: "import.editMapping", defaultMessage: "Edit mapping" },
  results: { id: "import.results", defaultMessage: "Import results" },
  export: { id: "import.export", defaultMessage: "Download error report" },
  startAgain: { id: "import.startAgain", defaultMessage: "Import another file" },
  invitations: { id: "import.invitations", defaultMessage: "Manage invitations" },
  row: { id: "import.row", defaultMessage: "Row" },
  decision: { id: "import.decision", defaultMessage: "Decision" },
  result: { id: "import.result", defaultMessage: "Result" },
  error: { id: "import.error", defaultMessage: "Issue" },
  skip: { id: "import.skip", defaultMessage: "Skip" },
  create: { id: "import.decision.create", defaultMessage: "Create" },
  update: { id: "import.decision.update", defaultMessage: "Update" },
  unchanged: { id: "import.decision.skip", defaultMessage: "Skip" },
  conflict: { id: "import.decision.conflict", defaultMessage: "Conflict" },
  pending: { id: "import.result.pending", defaultMessage: "Pending" },
  created: { id: "import.result.created", defaultMessage: "Created" },
  updated: { id: "import.result.updated", defaultMessage: "Updated" },
  skipped: { id: "import.result.skipped", defaultMessage: "Skipped" },
  failed: { id: "import.result.failed", defaultMessage: "Failed" },
  forbidden: {
    id: "import.forbidden",
    defaultMessage: "You don't have permission to import people.",
  },
  invalid: {
    id: "import.invalid",
    defaultMessage: "Check the file limits and column mapping, then try again.",
  },
  stale: {
    id: "import.stale",
    defaultMessage: "The import changed or needs a new preview. Upload corrected rows to retry.",
  },
  rowError: {
    id: "import.rowError",
    defaultMessage:
      "Review this row and its permissions, references and field formats in the original file.",
  },
  column_count: {
    id: "import.error.columnCount",
    defaultMessage: "Column count differs from the header.",
  },
  cell_limit: { id: "import.error.cellLimit", defaultMessage: "A cell exceeds the text limit." },
  formula: {
    id: "import.error.formula",
    defaultMessage: "Formula cells are not supported. Replace them with values.",
  },
  cell_error: {
    id: "import.error.cellError",
    defaultMessage: "The worksheet contains a cell error.",
  },
  duplicate_identity: {
    id: "import.error.duplicateIdentity",
    defaultMessage: "An identifier appears in multiple rows.",
  },
  identity_conflict: {
    id: "import.error.identityConflict",
    defaultMessage: "Identifiers or references match different people.",
  },
  stale_preview: {
    id: "import.error.stalePreview",
    defaultMessage: "Data changed after preview. Upload this row again.",
  },
  permission_denied: {
    id: "import.error.permissionDenied",
    defaultMessage: "You cannot change this person's grant or an invitation destination.",
  },
  invitation_delivery_unavailable: {
    id: "import.error.invitationDeliveryUnavailable",
    defaultMessage: "Email cannot safely deliver this grant. Turn off email delivery.",
  },
  invalid_fields: {
    id: "import.error.invalidFields",
    defaultMessage: "A mapped field value is invalid.",
  },
  reference_not_found: {
    id: "import.error.referenceNotFound",
    defaultMessage: "A role, team, manager or field could not be found.",
  },
  inactive_reference: {
    id: "import.error.inactiveReference",
    defaultMessage: "A person or team is no longer active.",
  },
});
const columnMessage = defineMessage<{ number: number }>({
  id: "import.column",
  defaultMessage: "Column {number}",
});
const summaryMessage = defineMessage<{
  count: number;
  create: number;
  update: number;
  skip: number;
  conflict: number;
}>({
  id: "import.summary",
  defaultMessage:
    "{count} rows: {create} create, {update} update, {skip} skip, {conflict} conflict.",
});
const completeMessage = defineMessage<{
  created: number;
  updated: number;
  skipped: number;
  failed: number;
}>({
  id: "import.complete",
  defaultMessage:
    "Finished: {created} created, {updated} updated, {skipped} skipped, {failed} failed.",
});
const skipMessage = defineMessage<{ number: number }>({
  id: "import.skipRow",
  defaultMessage: "Skip row {number}",
});

const baseTargets = [
  "username",
  "work_email",
  "contact_email",
  "display_name",
  "title",
  "teams",
  "role",
  "manager_email",
  "external_id",
] as const;
type Run = Schema<"PeopleImport">;
type Mapping = Schema<"PeopleImportMapping">;

export function PeopleImportPage({ runId }: { runId?: string | undefined }) {
  const intl = useIntl();
  const api = useApi();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const me = useMe();
  const endSession = useEndSession();
  const allowed = me.permissions.includes("people.manage");
  const [source, setSource] = useState("file");
  const [organization, setOrganization] = useState("");
  const [token, setToken] = useState("");
  const [fetching, setFetching] = useState(false);
  const currentFetch = useRef<AbortController | null>(null);
  useEffect(() => {
    setFetching(false);
    // A resumed run has no credential form; discard any previous form value.
    if (runId) setToken("");
    return () => {
      currentFetch.current?.abort();
      currentFetch.current = null;
    };
  }, [runId]);
  const [fetchError, setFetchError] = useState<unknown>();
  const [file, setFile] = useState<File>();
  const [run, setRun] = useState<Run>();
  const [mapping, setMapping] = useState<Mapping>({
    columns: [],
    createTeams: false,
    sendInvitations: false,
    skipRows: [],
  });
  const [editing, setEditing] = useState(true);
  const [needsPreview, setNeedsPreview] = useState(false);
  const resume = useQuery({
    queryKey: ["people-import", runId],
    staleTime: Number.POSITIVE_INFINITY,
    enabled: allowed && !!runId,
    queryFn: () =>
      call(
        api.client.GET("/api/v1/people-imports/{runId}", {
          params: { path: { runId: runId ?? "" } },
        }),
      ),
  });
  useEffect(() => {
    if (!resume.data) return;
    const next = resume.data;
    setRun(next);
    setMapping(
      next.state === "staged"
        ? {
            ...next.mapping,
            columns: next.headers.flatMap((h, column) => (h ? [{ column, target: h }] : [])),
          }
        : next.mapping,
    );
    setEditing(next.state === "staged");
    setNeedsPreview(false);
  }, [resume.data]);
  const fields = useQuery({
    queryKey: ["profile-fields", "import"],
    enabled: allowed,
    queryFn: () => call(api.client.GET("/api/v1/profile-fields")),
  });
  const upload = useMutation({
    mutationFn: async () => {
      if (!file) return;
      // The generated binary type is string; send the original File bytes.
      const next = await call(
        api.client.POST("/api/v1/people-imports", {
          body: file as unknown as string,
          bodySerializer: () => file,
          headers: { "Content-Type": "application/octet-stream" },
        }),
      );
      preview.reset();
      apply.reset();
      download.reset();
      queryClient.setQueryData(["people-import", next.id], next);
      setRun(next);
      setMapping({
        ...next.mapping,
        columns: next.headers.flatMap((h, column) => (h ? [{ column, target: h }] : [])),
      });
      setEditing(true);
      setNeedsPreview(false);
      await navigate({ to: "/people/import", search: { runId: next.id }, replace: true });
    },
  });
  const preview = useMutation({
    mutationFn: async () => {
      if (!run) return;
      const next = await call(
        api.client.POST("/api/v1/people-imports/{runId}/preview", {
          params: { path: { runId: run.id } },
          body: mapping,
        }),
      );
      apply.reset();
      download.reset();
      queryClient.setQueryData(["people-import", next.id], next);
      setRun(next);
      setEditing(false);
      setNeedsPreview(false);
    },
  });
  const apply = useMutation({
    mutationFn: async () => {
      if (!run) return;
      const next = await call(
        api.client.POST("/api/v1/people-imports/{runId}/apply", {
          params: { path: { runId: run.id } },
        }),
      );
      queryClient.setQueryData(["people-import", next.id], next);
      setRun(next);
    },
  });
  const download = useMutation({
    mutationFn: async (kind: "template" | "errors") => {
      const text =
        kind === "template"
          ? await call(api.client.GET("/api/v1/people-imports/template", { parseAs: "text" }))
          : run
            ? await call(
                api.client.GET("/api/v1/people-imports/{runId}/errors", {
                  params: { path: { runId: run.id } },
                  parseAs: "text",
                }),
              )
            : "";
      const url = URL.createObjectURL(new Blob([text], { type: "text/csv;charset=utf-8" }));
      const a = document.createElement("a");
      a.href = url;
      a.download = kind === "template" ? "people-template.csv" : "import-errors.csv";
      a.click();
      URL.revokeObjectURL(url);
    },
  });
  // Credentials never enter React Query mutation variables or retained data.
  const fetchGitHub = async () => {
    if (currentFetch.current) return;
    const controller = new AbortController();
    currentFetch.current = controller;
    const isCurrent = () => currentFetch.current === controller && !controller.signal.aborted;
    setFetching(true);
    setFetchError(undefined);
    const credential = token;
    setToken("");
    try {
      const next = await call(
        api.client.POST("/api/v1/people-imports/github", {
          body: { organization, token: credential },
          signal: controller.signal,
        }),
      );
      if (!isCurrent()) return;
      upload.reset();
      preview.reset();
      apply.reset();
      download.reset();
      queryClient.setQueryData(["people-import", next.id], next);
      setRun(next);
      setMapping({
        ...next.mapping,
        columns: next.headers.flatMap((h, column) => (h ? [{ column, target: h }] : [])),
      });
      setEditing(true);
      setNeedsPreview(false);
      await navigate({ to: "/people/import", search: { runId: next.id }, replace: true });
    } catch (err) {
      if (!isCurrent()) return;
      if (hasStatus(err, 401)) {
        await endSession("expired");
      } else {
        // Retain only the category/status needed for localized feedback.
        // Problem details, network causes and request diagnostics may be untrusted.
        setFetchError(
          err instanceof ApiError
            ? new ApiError(
                { type: "about:blank", title: "Import request failed", status: err.status },
                err.retryAfter,
              )
            : err instanceof NetworkError
              ? new NetworkError("Import network request failed")
              : new Error("Import request failed"),
        );
      }
    } finally {
      if (currentFetch.current === controller) {
        currentFetch.current = null;
        setFetching(false);
        setToken("");
      }
    }
  };
  const busy = fetching || upload.isPending || preview.isPending || apply.isPending;
  const error =
    fetchError ??
    upload.error ??
    preview.error ??
    apply.error ??
    download.error ??
    fields.error ??
    resume.error;
  const mapLabel = (target: string) =>
    target === "title"
      ? intl.formatMessage(messages.titleField)
      : target in messages
        ? intl.formatMessage(messages[target as keyof typeof messages])
        : target;
  const decisionLabel = (decision: string) =>
    intl.formatMessage(
      decision === "skip"
        ? messages.unchanged
        : decision === "create"
          ? messages.create
          : decision === "update"
            ? messages.update
            : messages.conflict,
    );
  const resultLabel = (result: string) =>
    intl.formatMessage(
      result === "created"
        ? messages.created
        : result === "updated"
          ? messages.updated
          : result === "skipped"
            ? messages.skipped
            : result === "failed"
              ? messages.failed
              : messages.pending,
    );
  const updateColumn = (column: number, target: string) =>
    setMapping((m) => ({
      ...m,
      columns: [
        ...m.columns.filter((c) => c.column !== column),
        ...(target ? [{ column, target }] : []),
      ],
    }));
  const count = (key: "decision" | "result", value: string) =>
    run?.rows.filter((r) => r[key] === value).length ?? 0;
  const noApply = !run?.rows.some((r) => r.decision !== "conflict");

  return (
    <Page
      title={intl.formatMessage(messages.title)}
      description={<FormattedMessage {...messages.description} />}
    >
      {!allowed ? (
        <Alert>
          <FormattedMessage {...messages.forbidden} />
        </Alert>
      ) : (
        <div className="flex max-w-4xl flex-col gap-6">
          {error && (
            <Alert role="alert">
              {errorMessage(intl, error, {
                400: fetchError ? messages.githubFailure : messages.invalid,
                403: messages.forbidden,
                409: messages.stale,
              })}
            </Alert>
          )}
          {!run ? (
            <Card className="flex flex-col gap-4 p-6">
              <label>
                <FormattedMessage {...messages.source} />
                <select
                  value={source}
                  disabled={busy}
                  onChange={(e) => {
                    setSource(e.target.value);
                    setToken("");
                    setFetchError(undefined);
                  }}
                >
                  <option value="file">{intl.formatMessage(messages.spreadsheet)}</option>
                  <option value="github">{intl.formatMessage(messages.github)}</option>
                </select>
              </label>
              {source === "github" ? (
                <>
                  <p>
                    <FormattedMessage {...messages.githubHint} />
                  </p>
                  <Field
                    label={<FormattedMessage {...messages.organization} />}
                    value={organization}
                    disabled={busy}
                    onChange={(e) => setOrganization(e.target.value)}
                    autoComplete="off"
                  />
                  <Field
                    type="password"
                    label={<FormattedMessage {...messages.token} />}
                    value={token}
                    disabled={busy}
                    onChange={(e) => setToken(e.target.value)}
                    autoComplete="off"
                  />
                  <Button
                    disabled={busy || !organization || !token}
                    loading={fetching}
                    aria-label={intl.formatMessage(messages.fetch)}
                    onClick={() => void fetchGitHub()}
                  >
                    <FormattedMessage {...messages.fetch} />
                  </Button>
                  {fetching && (
                    <p role="status">
                      <FormattedMessage {...messages.githubPending} />
                    </p>
                  )}
                </>
              ) : (
                <>
                  <Button variant="outline" onClick={() => download.mutate("template")}>
                    <FormattedMessage {...messages.template} />
                  </Button>
                  <Field
                    type="file"
                    disabled={busy}
                    accept=".csv,.xlsx"
                    label={<FormattedMessage {...messages.file} />}
                    hint={<FormattedMessage {...messages.limits} />}
                    onChange={(e) => {
                      setFile(e.target.files?.[0]);
                      upload.reset();
                    }}
                  />
                  <Button
                    disabled={!file || file.size > 5 * 1024 * 1024 || busy}
                    loading={upload.isPending}
                    aria-label={intl.formatMessage(messages.upload)}
                    onClick={() => upload.mutate()}
                  >
                    <FormattedMessage {...messages.upload} />
                  </Button>
                </>
              )}
            </Card>
          ) : editing && run.state !== "applied" ? (
            <Card className="flex flex-col gap-4 p-6">
              <h2 className="text-lg font-semibold">
                <FormattedMessage {...messages.mapping} />
              </h2>
              <p className="text-sm text-muted-foreground">
                <FormattedMessage {...messages.mappingHint} />
              </p>
              <p className="text-sm text-muted-foreground">
                <FormattedMessage {...messages.createHint} />
              </p>
              {run.headers.map((header, column) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: source column positions are immutable identifiers within a run.
                <label key={column} className="grid gap-2 sm:grid-cols-2">
                  <span>
                    {header
                      ? mapLabel(header)
                      : intl.formatMessage(columnMessage, { number: column + 1 })}
                  </span>
                  <select
                    disabled={busy}
                    className="rounded-md border bg-background px-3 py-2 text-sm"
                    aria-label={intl.formatMessage(columnMessage, { number: column + 1 })}
                    value={mapping.columns.find((c) => c.column === column)?.target ?? ""}
                    onChange={(e) => updateColumn(column, e.target.value)}
                  >
                    <option value="">{intl.formatMessage(messages.ignore)}</option>
                    {baseTargets.map((target) => (
                      <option key={target} value={target}>
                        {mapLabel(target)}
                      </option>
                    ))}
                    {fields.data?.items
                      .filter(
                        (f) =>
                          !f.archived &&
                          (!f.sensitive || me.permissions.includes("people.sensitive.read")),
                      )
                      .map((f) => (
                        <option key={f.key} value={`profile.${f.key}`}>
                          {f.label}
                        </option>
                      ))}
                  </select>
                </label>
              ))}
              <label className="flex gap-2">
                <input
                  type="checkbox"
                  disabled={busy}
                  checked={mapping.createTeams}
                  onChange={(e) => setMapping((m) => ({ ...m, createTeams: e.target.checked }))}
                />
                <FormattedMessage {...messages.createTeams} />
              </label>
              <label className="flex gap-2">
                <input
                  type="checkbox"
                  disabled={busy}
                  checked={mapping.sendInvitations}
                  onChange={(e) => setMapping((m) => ({ ...m, sendInvitations: e.target.checked }))}
                />
                <FormattedMessage {...messages.sendInvitations} />
              </label>
              <p className="text-sm text-muted-foreground">
                <FormattedMessage {...messages.invitationHint} />
              </p>
              <Button
                disabled={busy || mapping.columns.length === 0}
                loading={preview.isPending}
                aria-label={intl.formatMessage(messages.preview)}
                onClick={() => preview.mutate()}
              >
                <FormattedMessage {...messages.preview} />
              </Button>
            </Card>
          ) : (
            <Card className="flex flex-col gap-4 p-6">
              <h2 className="text-lg font-semibold">
                <FormattedMessage
                  {...(run.state === "applied" ? messages.results : messages.previewTitle)}
                />
              </h2>
              <p role="status">
                {run.state === "applied"
                  ? intl.formatMessage(completeMessage, {
                      created: count("result", "created"),
                      updated: count("result", "updated"),
                      skipped: count("result", "skipped"),
                      failed: count("result", "failed"),
                    })
                  : intl.formatMessage(summaryMessage, {
                      count: run.rows.length,
                      create: count("decision", "create"),
                      update: count("decision", "update"),
                      skip: count("decision", "skip"),
                      conflict: count("decision", "conflict"),
                    })}
              </p>
              <p className="text-sm text-muted-foreground">
                <FormattedMessage {...messages.privacy} />
              </p>
              <div className="max-h-96 overflow-auto">
                <table className="w-full text-left text-sm">
                  <thead>
                    <tr>
                      <th>
                        <FormattedMessage {...messages.row} />
                      </th>
                      <th>
                        <FormattedMessage {...messages.decision} />
                      </th>
                      <th>
                        <FormattedMessage {...messages.error} />
                      </th>
                      <th>
                        <FormattedMessage
                          {...(run.state === "applied" ? messages.result : messages.skip)}
                        />
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {run.rows.map((row) => (
                      <tr key={row.row} className="border-t">
                        <td className="p-2">{row.row}</td>
                        <td className="p-2">{decisionLabel(row.decision)}</td>
                        <td className="p-2">
                          {row.error
                            ? intl.formatMessage(
                                messages[row.error as keyof typeof messages] ?? messages.rowError,
                              )
                            : null}
                        </td>
                        <td className="p-2">
                          {run.state === "applied" ? (
                            resultLabel(row.result)
                          ) : (
                            <input
                              type="checkbox"
                              disabled={busy}
                              aria-label={intl.formatMessage(skipMessage, { number: row.row })}
                              checked={mapping.skipRows.includes(row.row)}
                              onChange={(e) => {
                                setMapping((m) => ({
                                  ...m,
                                  skipRows: e.target.checked
                                    ? [...m.skipRows, row.row]
                                    : m.skipRows.filter((n) => n !== row.row),
                                }));
                                setNeedsPreview(true);
                              }}
                            />
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {run.state === "applied" ? (
                <>
                  <Button variant="outline" onClick={() => download.mutate("errors")}>
                    <FormattedMessage {...messages.export} />
                  </Button>
                  <Link to="/people/invitations" className="text-sm underline">
                    <FormattedMessage {...messages.invitations} />
                  </Link>
                  <Button
                    onClick={() => {
                      setRun(undefined);
                      setFile(undefined);
                      upload.reset();
                      preview.reset();
                      apply.reset();
                      download.reset();
                      void navigate({ to: "/people/import", search: {}, replace: true });
                    }}
                  >
                    <FormattedMessage {...messages.startAgain} />
                  </Button>
                </>
              ) : (
                <div className="flex flex-wrap gap-3">
                  {needsPreview ? (
                    <Button
                      disabled={busy}
                      loading={preview.isPending}
                      aria-label={intl.formatMessage(messages.preview)}
                      onClick={() => preview.mutate()}
                    >
                      <FormattedMessage {...messages.preview} />
                    </Button>
                  ) : (
                    <Button
                      disabled={busy || noApply}
                      loading={apply.isPending}
                      aria-label={intl.formatMessage(messages.apply)}
                      onClick={() => apply.mutate()}
                    >
                      <FormattedMessage {...messages.apply} />
                    </Button>
                  )}
                  <Button variant="outline" disabled={busy} onClick={() => setEditing(true)}>
                    <FormattedMessage {...messages.editMapping} />
                  </Button>
                </div>
              )}
            </Card>
          )}
        </div>
      )}
    </Page>
  );
}
