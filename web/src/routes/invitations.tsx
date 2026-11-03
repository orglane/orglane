import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Field } from "@/components/field";
import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { call, type Schema } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";
import { useMe } from "@/lib/session";
import { teamsKey, teamsQuery } from "@/lib/teams";

export const invitationsKey = ["invitations"] as const;
const messages = defineMessages({
  import: { id: "invitations.import", defaultMessage: "Import people" },
  title: { id: "invitations.title", defaultMessage: "Invitations" },
  description: {
    id: "invitations.description",
    defaultMessage: "Invite people to your workspace and manage their invitations.",
  },
  name: { id: "invitations.name", defaultMessage: "Full name" },
  contact: { id: "invitations.contact", defaultMessage: "Contact email" },
  contactHint: {
    id: "invitations.contactHint",
    defaultMessage: "A reachable address for invitations and recovery. Separate from work email.",
  },
  username: { id: "invitations.username", defaultMessage: "Username" },
  workEmail: { id: "invitations.workEmail", defaultMessage: "Work email" },
  workHint: {
    id: "invitations.workHint",
    defaultMessage: "Their identity in the workspace. Edit to use a different address.",
  },
  role: { id: "invitations.role", defaultMessage: "Role" },
  member: { id: "invitations.role.member", defaultMessage: "Member" },
  admin: { id: "invitations.role.admin", defaultMessage: "Admin" },
  owner: { id: "invitations.role.owner", defaultMessage: "Owner" },
  invite: { id: "invitations.invite", defaultMessage: "Invite person" },
  sent: { id: "invitations.sent", defaultMessage: "Invitation created." },
  linkMode: {
    id: "invitations.linkMode",
    defaultMessage:
      "System email is not configured. Share the invitation link with the person yourself.",
  },
  emailMode: {
    id: "invitations.emailMode",
    defaultMessage:
      "Invitations are emailed to the contact address. Delivery status appears below.",
  },
  restrictedLink: {
    id: "invitations.restrictedLink",
    defaultMessage:
      "Share this invitation link directly. Email settings editable by workspace admins cannot safely carry this role's additional permissions.",
  },
  link: { id: "invitations.link", defaultMessage: "Invitation link" },
  copy: { id: "invitations.copy", defaultMessage: "Copy link" },
  copied: { id: "invitations.copied", defaultMessage: "Copied" },
  copyFailed: {
    id: "invitations.copyFailed",
    defaultMessage: "Select the link and copy it manually.",
  },
  linkHint: {
    id: "invitations.linkHint",
    defaultMessage:
      "Shown only now. Resend to create a new link; the previous link will stop working.",
  },
  empty: { id: "invitations.empty", defaultMessage: "No pending invitations." },
  resend: { id: "invitations.resend", defaultMessage: "Resend" },
  revoke: { id: "invitations.revoke", defaultMessage: "Revoke" },
  pending: { id: "invitations.status.pending", defaultMessage: "Pending acceptance" },
  expired: { id: "invitations.status.expired", defaultMessage: "Expired" },
  revoked: { id: "invitations.status.revoked", defaultMessage: "Revoked" },
  queued: { id: "invitations.email.queued", defaultMessage: "Email queued" },
  retrying: { id: "invitations.email.retrying", defaultMessage: "Email delivery failed; retrying" },
  delivered: { id: "invitations.email.sent", defaultMessage: "Email sent" },
  failed: {
    id: "invitations.email.failed",
    defaultMessage: "Email delivery failed. Check email settings and resend.",
  },
  canceled: { id: "invitations.email.canceled", defaultMessage: "Email canceled" },
  expires: defineMessage<{ date: string }>({
    id: "invitations.expires",
    defaultMessage: "Link expires {date}",
  }),
  settings: { id: "invitations.settings", defaultMessage: "Email settings" },
  forbidden: {
    id: "invitations.forbidden",
    defaultMessage: "You don't have permission to manage invitations.",
  },
  invalid: {
    id: "invitations.invalid",
    defaultMessage: "Check the name, addresses, username and role, then try again.",
  },
  conflict: {
    id: "invitations.conflict",
    defaultMessage:
      "That username or work email is already in use, or the invitation was already accepted.",
  },
  teams: { id: "invitations.teams", defaultMessage: "Teams" },
  teamsHint: {
    id: "invitations.teamsHint",
    defaultMessage: "Choose the teams this person joins as a member.",
  },
  noTeams: { id: "invitations.noTeams", defaultMessage: "No active teams yet." },
  retry: { id: "common.retry", defaultMessage: "Try again" },
});

function InvitationLink({ link }: { link: string }) {
  const intl = useIntl();
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);
  return (
    <Card className="space-y-3 p-5" role="status">
      <Field
        label={intl.formatMessage(messages.link)}
        value={link}
        readOnly
        onFocus={(e) => e.target.select()}
        hint={intl.formatMessage(messages.linkHint)}
      />
      <Button
        variant="outline"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(link);
            setCopied(true);
          } catch {
            setFailed(true);
          }
        }}
      >
        <FormattedMessage {...(copied ? messages.copied : messages.copy)} />
      </Button>
      {failed && (
        <p>
          <FormattedMessage {...messages.copyFailed} />
        </p>
      )}
    </Card>
  );
}

function InvitationRow({
  invitation: inv,
  onIssued,
  canManage,
}: {
  invitation: Schema<"Invitation">;
  onIssued: (link?: string) => void;
  canManage: boolean;
}) {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const refresh = () => queryClient.invalidateQueries({ queryKey: invitationsKey });
  const resend = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/invitations/{invitationId}/resend", {
          params: { path: { invitationId: inv.id } },
        }),
      ),
    onSuccess: async (result) => {
      onIssued(result.link);
      await refresh();
    },
  });
  const revoke = useMutation({
    mutationFn: () =>
      call(
        api.client.DELETE("/api/v1/invitations/{invitationId}", {
          params: { path: { invitationId: inv.id } },
        }),
      ),
    onSuccess: async () => {
      onIssued();
      await refresh();
    },
  });
  const emailStates = {
    queued: messages.queued,
    retrying: messages.retrying,
    sent: messages.delivered,
    failed: messages.failed,
    canceled: messages.canceled,
  };
  const error = resend.error ?? revoke.error;
  return (
    <li className="space-y-3 p-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0 space-y-1">
          <h2 className="font-semibold">{inv.displayName}</h2>
          <p className="break-words text-muted-foreground text-sm">{inv.contactEmail}</p>
          <p className="break-words text-sm">{inv.workEmail}</p>
          <p className="text-sm">
            <FormattedMessage {...messages[inv.status]} />
          </p>
          <p className="text-muted-foreground text-xs">
            <FormattedMessage
              {...messages.expires}
              values={{
                date: intl.formatDate(inv.expiresAt, { dateStyle: "medium", timeStyle: "short" }),
              }}
            />
          </p>
        </div>
        {canManage && (
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              loading={resend.isPending}
              disabled={revoke.isPending}
              onClick={() => resend.mutate()}
            >
              <FormattedMessage {...messages.resend} />
            </Button>
            {inv.status !== "revoked" && (
              <Button
                variant="destructive-ghost"
                size="sm"
                loading={revoke.isPending}
                disabled={resend.isPending}
                onClick={() => revoke.mutate()}
              >
                <FormattedMessage {...messages.revoke} />
              </Button>
            )}
          </div>
        )}
      </div>
      {inv.email && (
        <div role={inv.email.lastError ? "alert" : "status"} className="text-sm">
          <p>
            <FormattedMessage {...emailStates[inv.email.status]} />
          </p>
          {inv.email.lastError && (
            <p className="mt-1 break-words text-muted-foreground">{inv.email.lastError}</p>
          )}
        </div>
      )}
      {error && <Alert>{errorMessage(intl, error, { 409: messages.conflict })}</Alert>}
    </li>
  );
}

export function InvitationsPage() {
  const intl = useIntl();
  const me = useMe();
  const allowed = me.permissions.includes("people.manage");
  return (
    <Page
      title={intl.formatMessage(messages.title)}
      description={intl.formatMessage(messages.description)}
      actions={
        <div className="flex gap-4">
          {allowed && (
            <Link to="/people/import" className="text-sm underline">
              <FormattedMessage {...messages.import} />
            </Link>
          )}
          {me.permissions.includes("instance.manage") && (
            <Link to="/settings/email" className="text-sm underline">
              <FormattedMessage {...messages.settings} />
            </Link>
          )}
        </div>
      }
    >
      {allowed ? (
        <Invitations />
      ) : (
        <Alert>
          <FormattedMessage {...messages.forbidden} />
        </Alert>
      )}
    </Page>
  );
}

function Invitations() {
  const intl = useIntl();
  const api = useApi();
  const queryClient = useQueryClient();
  const [values, setValues] = useState({
    displayName: "",
    contactEmail: "",
    username: "",
    workEmail: "",
    role: "member",
  });
  const [workEdited, setWorkEdited] = useState(false);
  const [teamIds, setTeamIds] = useState<string[]>([]);
  const [link, setLink] = useState<string>();
  const options = useQuery({
    queryKey: ["invitation-options"],
    queryFn: () => call(api.client.GET("/api/v1/invitations/options")),
  });
  const invitations = useQuery({
    queryKey: invitationsKey,
    queryFn: () => call(api.client.GET("/api/v1/invitations")),
    refetchInterval: 3000,
  });
  const teams = useQuery(teamsQuery(api));
  const workEmail = workEdited
    ? values.workEmail
    : (options.data?.workEmailPattern ?? "").replace("{username}", values.username.toLowerCase());
  const invite = useMutation({
    mutationFn: () =>
      call(api.client.POST("/api/v1/invitations", { body: { ...values, workEmail, teamIds } })),
    onSuccess: async (result) => {
      setLink(result.link);
      setValues({ displayName: "", contactEmail: "", username: "", workEmail: "", role: "member" });
      setWorkEdited(false);
      setTeamIds([]);
      await queryClient.invalidateQueries({ queryKey: invitationsKey });
      await queryClient.invalidateQueries({ queryKey: teamsKey });
    },
  });
  const error = invite.error ?? options.error ?? invitations.error ?? teams.error;
  return (
    <div className="space-y-6">
      {options.data && (
        <p className="text-muted-foreground text-sm">
          <FormattedMessage
            {...(options.data.delivery === "link"
              ? messages.linkMode
              : options.data.roleDeliveries?.[values.role] === "link"
                ? messages.restrictedLink
                : messages.emailMode)}
          />
        </p>
      )}
      <Card className="p-5 sm:p-6">
        <form
          className="grid gap-5 sm:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault();
            setLink(undefined);
            invite.mutate();
          }}
        >
          <Field
            label={intl.formatMessage(messages.name)}
            autoComplete="off"
            required
            maxLength={200}
            value={values.displayName}
            onChange={(e) => setValues({ ...values, displayName: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.contact)}
            hint={intl.formatMessage(messages.contactHint)}
            type="email"
            required
            value={values.contactEmail}
            onChange={(e) => setValues({ ...values, contactEmail: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.username)}
            required
            pattern="[a-zA-Z0-9][a-zA-Z0-9._\-]*"
            maxLength={63}
            value={values.username}
            onChange={(e) => setValues({ ...values, username: e.target.value })}
          />
          <Field
            label={intl.formatMessage(messages.workEmail)}
            hint={intl.formatMessage(messages.workHint)}
            type="email"
            required
            value={workEmail}
            onChange={(e) => {
              setWorkEdited(true);
              setValues({ ...values, workEmail: e.target.value });
            }}
          />
          <label className="flex flex-col gap-2 font-medium text-sm">
            <FormattedMessage {...messages.role} />
            <select
              className="h-10 rounded-md border bg-surface px-3"
              value={values.role}
              onChange={(e) => setValues({ ...values, role: e.target.value })}
            >
              <option value="member">{intl.formatMessage(messages.member)}</option>
              {options.data?.roles.includes("admin") && (
                <option value="admin">{intl.formatMessage(messages.admin)}</option>
              )}
              {options.data?.roles.includes("owner") && (
                <option value="owner">{intl.formatMessage(messages.owner)}</option>
              )}
            </select>
          </label>
          <fieldset className="space-y-3 sm:col-span-2">
            <legend className="font-medium text-sm">
              <FormattedMessage {...messages.teams} />
            </legend>
            <p className="text-muted-foreground text-sm">
              <FormattedMessage {...messages.teamsHint} />
            </p>
            <div className="flex flex-wrap gap-4">
              {teams.data?.items.map((team) => (
                <label key={team.id} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={teamIds.includes(team.id)}
                    onChange={(e) =>
                      setTeamIds(
                        e.target.checked
                          ? [...teamIds, team.id]
                          : teamIds.filter((id) => id !== team.id),
                      )
                    }
                  />
                  {team.name}
                </label>
              ))}
            </div>
            {teams.data?.items.length === 0 && (
              <p className="text-muted-foreground text-sm">
                <FormattedMessage {...messages.noTeams} />
              </p>
            )}
          </fieldset>
          <div className="flex items-end">
            <Button type="submit" loading={invite.isPending} disabled={!options.data}>
              <FormattedMessage {...messages.invite} />
            </Button>
          </div>
        </form>
      </Card>
      {error && (
        <Alert>
          {errorMessage(intl, error, { 400: messages.invalid, 409: messages.conflict })}
        </Alert>
      )}
      {invite.isSuccess && (
        <p role="status">
          <FormattedMessage {...messages.sent} />
        </p>
      )}
      {link && <InvitationLink key={link} link={link} />}
      {invitations.isError && (
        <Button variant="outline" onClick={() => invitations.refetch()}>
          <FormattedMessage {...messages.retry} />
        </Button>
      )}
      {invitations.data && (
        <Card>
          <ul className="divide-y">
            {invitations.data.items.map((inv) => (
              <InvitationRow
                key={inv.id}
                invitation={inv}
                onIssued={setLink}
                canManage={inv.roles.every((role) => options.data?.roles.includes(role))}
              />
            ))}
          </ul>
          {invitations.data.items.length === 0 && (
            <p className="p-5 text-muted-foreground text-sm">
              <FormattedMessage {...messages.empty} />
            </p>
          )}
        </Card>
      )}
    </div>
  );
}
