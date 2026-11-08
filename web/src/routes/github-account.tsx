import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { defineMessage, defineMessages, FormattedMessage, useIntl } from "react-intl";

import { Page } from "@/components/page";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { ApiError, call } from "@/lib/api";
import { useApi } from "@/lib/api-context";
import { errorMessage } from "@/lib/errors";

const m = defineMessages({
  title: { id: "githubAccount.title", defaultMessage: "Link your GitHub account" },
  description: {
    id: "githubAccount.description",
    defaultMessage:
      "Sign in to GitHub once so Orglane knows which GitHub account is yours. It never posts anything as you and does not keep access to your account.",
  },
  loading: { id: "common.loading", defaultMessage: "Loading" },
  notConnected: {
    id: "githubAccount.notConnected",
    defaultMessage:
      "Your company has not connected GitHub to Orglane yet. There is nothing to link for now.",
  },
  linked: defineMessage<{ login: string }>({
    id: "githubAccount.linked",
    defaultMessage: "Linked to the GitHub account @{login}.",
  }),
  notLinked: { id: "githubAccount.notLinked", defaultMessage: "No GitHub account linked yet." },
  expected: defineMessage<{ login: string }>({
    id: "githubAccount.expected",
    defaultMessage:
      "Your administrator expects the GitHub account @{login}. Link whichever account is yours; this hint proves nothing.",
  }),
  expectedConflict: defineMessage<{ login: string }>({
    id: "githubAccount.expectedConflict",
    defaultMessage:
      "The expected account @{login} is already linked to someone else. If it is yours, ask an administrator.",
  }),
  link: { id: "githubAccount.link", defaultMessage: "Link GitHub account" },
  relink: { id: "githubAccount.relink", defaultMessage: "Link a different account" },
  redirecting: { id: "githubAccount.redirecting", defaultMessage: "Opening GitHub…" },
  unlink: { id: "githubAccount.unlink", defaultMessage: "Unlink" },
  unlinkConfirm: defineMessage<{ login: string }>({
    id: "githubAccount.unlinkConfirm",
    defaultMessage:
      "Unlink @{login}? Orglane stops managing this account. Access it already has on GitHub is not removed.",
  }),
  unlinkYes: { id: "githubAccount.unlinkYes", defaultMessage: "Unlink account" },
  cancel: { id: "common.cancel", defaultMessage: "Cancel" },
  verified: { id: "personApps.verified", defaultMessage: "Verified" },
  // Callback outcomes.
  completing: { id: "githubAccount.completing", defaultMessage: "Confirming your GitHub account…" },
  done: { id: "githubAccount.done", defaultMessage: "GitHub account linked" },
  doneDescription: defineMessage<{ login: string }>({
    id: "githubAccount.doneDescription",
    defaultMessage: "Orglane now knows that @{login} is your GitHub account.",
  }),
  denied: {
    id: "githubAccount.denied",
    defaultMessage: "You did not authorize on GitHub, so nothing was linked. You can try again.",
  },
  invalidState: {
    id: "githubAccount.invalidState",
    defaultMessage:
      "This link attempt expired or was started in another browser or session. Start again from this browser.",
  },
  alreadyLinked: {
    id: "githubAccount.alreadyLinked",
    defaultMessage:
      "This GitHub account is already linked to another person in Orglane. If it is yours, ask an administrator to remove that link, then try again.",
  },
  failed: {
    id: "githubAccount.failed",
    defaultMessage: "GitHub did not confirm your account. Try again.",
  },
  back: { id: "githubAccount.back", defaultMessage: "Back to GitHub account" },
});

const accountLinksKey = ["me", "account-links"] as const;

/** The "Link your GitHub account" employee step (/me/accounts/github). */
export function GitHubAccountPage() {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const links = useQuery({
    queryKey: accountLinksKey,
    queryFn: () => call(api.client.GET("/api/v1/me/account-links", {})),
  });
  const start = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/me/account-links/{provider}/start", {
          params: { path: { provider: "github" } },
          body: {},
        }),
      ),
    onSuccess: (data) => window.location.assign(data.url),
  });
  const unlink = useMutation({
    mutationFn: (linkId: string) =>
      call(
        api.client.DELETE("/api/v1/me/account-links/{linkId}", {
          params: { path: { linkId } },
        }),
      ),
    onSuccess: () => {
      setConfirming(false);
      return queryClient.invalidateQueries({ queryKey: accountLinksKey });
    },
  });
  const data = links.data;
  const verified = data?.links.find((l) => l.provider === "github" && l.state === "verified");
  const expected = data?.links.filter((l) => l.provider === "github" && l.state === "pending");
  const linkable = data?.linkable.some((p) => p.provider === "github");
  return (
    <Page title={intl.formatMessage(m.title)} description={intl.formatMessage(m.description)}>
      <Card>
        <CardHeader>
          <CardTitle>
            <FormattedMessage {...m.title} />
          </CardTitle>
          <CardDescription>
            {verified ? (
              <FormattedMessage {...m.linked} values={{ login: verified.login }} />
            ) : (
              <FormattedMessage {...m.notLinked} />
            )}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {links.isPending && (
            <p role="status">
              <FormattedMessage {...m.loading} />
            </p>
          )}
          {links.isError && <Alert>{errorMessage(intl, links.error)}</Alert>}
          {data && !linkable && !verified && (
            <Alert>
              <FormattedMessage {...m.notConnected} />
            </Alert>
          )}
          {verified && (
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium [overflow-wrap:anywhere]">@{verified.login}</span>
              <Badge variant="success">
                <FormattedMessage {...m.verified} />
              </Badge>
            </div>
          )}
          {!verified &&
            expected?.map((l) => (
              <p
                key={`${l.organizationId}-${l.externalId}`}
                className="text-muted-foreground text-sm [overflow-wrap:anywhere]"
              >
                <FormattedMessage
                  {...(l.conflict ? m.expectedConflict : m.expected)}
                  values={{ login: l.login }}
                />
              </p>
            ))}
          {start.isError && <Alert>{errorMessage(intl, start.error)}</Alert>}
          {unlink.isError && <Alert>{errorMessage(intl, unlink.error)}</Alert>}
          {confirming && verified ? (
            // biome-ignore lint/a11y/useSemanticElements: an inline confirmation, not a fieldset
            <div
              role="group"
              aria-label={intl.formatMessage(m.unlink)}
              className="space-y-2 rounded-md border border-destructive/40 p-3"
            >
              <p>
                <FormattedMessage {...m.unlinkConfirm} values={{ login: verified.login }} />
              </p>
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="ghost"
                  autoFocus
                  disabled={unlink.isPending}
                  onClick={() => setConfirming(false)}
                >
                  <FormattedMessage {...m.cancel} />
                </Button>
                <Button
                  size="sm"
                  variant="destructive"
                  loading={unlink.isPending}
                  onClick={() => verified.id && unlink.mutate(verified.id)}
                >
                  <FormattedMessage {...m.unlinkYes} />
                </Button>
              </div>
            </div>
          ) : (
            <div className="flex flex-wrap gap-2">
              {linkable && (
                <Button
                  variant={verified ? "outline" : "primary"}
                  loading={start.isPending || start.isSuccess}
                  onClick={() => start.mutate()}
                >
                  <FormattedMessage {...(verified ? m.relink : m.link)} />
                </Button>
              )}
              {verified && (
                <Button variant="destructive-ghost" onClick={() => setConfirming(true)}>
                  <FormattedMessage {...m.unlink} />
                </Button>
              )}
            </div>
          )}
          {start.isSuccess && (
            <p role="status" className="text-muted-foreground text-sm">
              <FormattedMessage {...m.redirecting} />
            </p>
          )}
        </CardContent>
      </Card>
    </Page>
  );
}

/** GitHub's redirect after authorization (/me/accounts/github/callback). */
export function GitHubAccountCallbackPage() {
  const api = useApi();
  const intl = useIntl();
  const queryClient = useQueryClient();
  const started = useRef(false);
  const [params] = useState(() => new URLSearchParams(window.location.search));
  const denied = params.get("error") === "access_denied";
  const invalid =
    ["state", "code"].some((key) => params.getAll(key).length !== 1) || params.has("error");
  const complete = useMutation({
    mutationFn: () =>
      call(
        api.client.POST("/api/v1/me/account-links/{provider}/complete", {
          params: { path: { provider: "github" } },
          body: { state: params.get("state") ?? "", code: params.get("code") ?? "" },
        }),
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: accountLinksKey }),
  });
  const { mutate } = complete;
  useEffect(() => {
    // Remove the one-time code and state from browser history first.
    window.history.replaceState(window.history.state, "", window.location.pathname);
    if (started.current) return;
    started.current = true;
    if (!invalid) mutate();
  }, [mutate, invalid]);
  const code = complete.error instanceof ApiError ? complete.error.problem.code : undefined;
  return (
    <Page title={intl.formatMessage(complete.data ? m.done : m.title)}>
      {complete.data ? (
        <Alert variant="success">
          <FormattedMessage {...m.doneDescription} values={{ login: complete.data.login }} />
        </Alert>
      ) : denied ? (
        <Alert>
          <FormattedMessage {...m.denied} />
        </Alert>
      ) : invalid || code === "invalid_state" ? (
        <Alert>
          <FormattedMessage {...m.invalidState} />
        </Alert>
      ) : code === "account_already_linked" ? (
        <Alert>
          <FormattedMessage {...m.alreadyLinked} />
        </Alert>
      ) : complete.isError ? (
        <Alert>
          {complete.error instanceof ApiError && complete.error.status === 429 ? (
            errorMessage(intl, complete.error)
          ) : (
            <FormattedMessage {...m.failed} />
          )}
        </Alert>
      ) : (
        <p role="status">
          <FormattedMessage {...m.completing} />
        </p>
      )}
      <Button asChild variant="link" className="mt-4">
        <Link to="/me/accounts/github">
          <FormattedMessage {...m.back} />
        </Link>
      </Button>
    </Page>
  );
}
