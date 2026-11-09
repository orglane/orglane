# Connect a GitHub organization

Orglane connects GitHub.com through a GitHub App owned by your organization.
You need `apps.manage` in Orglane (Owner or Admin) and permission to create and
install an App in the target GitHub organization. GitHub Enterprise Server and
Enterprise Managed Users are outside M2 scope.

## Guided setup

1. Open **Settings → Apps → Connect GitHub** in a signed-in browser. Enter the
   organization's login (not a URL) and a unique Orglane app key.
2. Select **Prepare GitHub App**, review the requested access, then **Continue
   to GitHub**. Orglane creates a `connecting` instance first, so the manifest's
   webhook address identifies that instance. Preparing it makes no GitHub API
   calls and grants nobody access.
3. Create the private App on GitHub. GitHub returns a one-time manifest code to
   `/settings/apps/github/callback`; Orglane exchanges it for App credentials
   and stores them sealed in `app_credentials`, never in settings or API responses.
4. Continue to GitHub to install that App on the intended organization. Select
   only the repositories Orglane should see. Return in the same browser session.
5. Orglane reads the installation with an App JWT, resolves the expected
   organization account, and checks the App ID, installation ID, account and
   target IDs/types, suspension state and required permissions. Only then does
   the instance become `active` and receive its `code` binding.

Use a reachable HTTPS workspace for real setup. Local HTTP works for preparing
and inspecting the manifest, but GitHub must accept the callback and webhook
addresses for real use. Hosts and URLs come from Orglane's domain configuration.

An active connection verifies setup and credentials. Employee account linking,
invitations, team mappings and removal are implemented; webhook ingestion
semantics are #66. The capability matrix reports each feature's support.

## Recovering an interrupted setup

Both browser legs have independent random, single-use states, stored as hashes,
bound to the initiating person, browser session, instance and step for one hour.
Current `apps.manage` permission is checked again on every API call. Foreign,
expired, used and unknown states all produce `invalid_state`; callback parameters
cannot choose another instance. A fresh resume invalidates previous states.
Temporary callback codes and state are removed from browser history immediately.

If the installation callback fails, return to the connecting instance and select
**Resume GitHub connection**. Stored credentials resume the installation leg;
no second App is created. If the manifest conversion succeeded at GitHub but
its response or the database commit was lost, Orglane cannot recover that
one-time response. Use the manual fallback for that App, or delete the unused
App on GitHub and prepare it again. A failed callback consumes its state before
any external call; it is never blindly replayed.

## Manual fallback

First prepare a connection and open **View connection**. Its overview shows the
instance-specific webhook URL. Before the App has been saved, **Use manual
setup** exposes the credential form on that connecting instance.

Create a private GitHub App under the intended organization's developer
settings, following [GitHub's registration instructions](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app):

- Homepage URL: your workspace origin.
- Setup URL: your workspace origin followed by `/settings/apps/github/callback`.
- User authorization callback URL: your workspace origin followed by
  `/me/accounts/github/callback` (account linking).
- Keep **Request user authorization (OAuth) during installation** off, so GitHub
  uses the Setup URL after installation.
- Active webhook URL: the exact webhook URL in the connecting instance overview.
  Generate a fresh webhook secret of at least 32 characters.
- Set the permissions and events below.
- After creating the App, on its settings page generate a private key
  (**Generate a private key**, a PEM file downloads) and a client secret
  (**Generate a new client secret**; GitHub shows it only once, so copy it
  directly into Orglane's form).
- Enter App ID, Client ID, private key PEM, Client secret and webhook secret in
  Orglane's manual form. Orglane verifies the key against `GET /app`, checks App
  ID and Client ID, seals the credentials, then offers the installation redirect.
  Client-secret use is checked by the manual OAuth acceptance.

The same public `POST /api/v1/apps/github/connect` API accepts `manual` alongside
`organization`, `key` and `instanceId`; no private admin endpoint is used.
Never put credentials into a shell command, issue, PR, screenshot or repository.

## Linking people's GitHub accounts

Each person proves their own GitHub account once: the onboarding step **Link
your GitHub account** (optional; at `/me/accounts/github`) sends them to
GitHub's authorization page for your App and back to
`/me/accounts/github/callback`. Orglane then knows their GitHub account by its
numeric user ID, never by login or email, uses the token only to read who
authorized, and revokes the authorization right away. The step appears for
people with access to a GitHub instance (with the default `mapped_teams`
policy, people in a mapped team).

- An account already linked to someone else cannot be linked again; an admin
  can revoke the wrong link on the person's page (**Apps and accounts**).
  Admins can revoke links, never create them.
- Linking another account replaces the person's own link; the old account is
  no longer managed (reported, never removed).
- Accounts from the GitHub people import (M1) are shown as expected
  accounts. They prove nothing: if the linked account differs, both are shown;
  if another person linked the expected account, it is marked as a conflict.

## Inviting people, teams and removal

A person with access to a GitHub instance is invited to the organization by
their linked account's immutable numeric user ID
(`POST /orgs/{org}/invitations` with `invitee_id`). Orglane never creates GitHub
accounts.

- The invitation is Orglane's only when GitHub confirms it, or when a pending
  invitation was made by the App's bot (`<app-slug>[bot]`). A membership that
  already existed, or an invitation a person made, proves nothing: that person
  is reported as unmanaged and never removed. Revoking or replacing the linked
  account ends Orglane's ownership of the old account (reported, never removed).
- Every action re-reads the account by its numeric ID and uses its current
  login only for that call. Immediately before a team membership change, an
  organization removal or an invitation cancellation it resolves the login
  from the ID again: if a rename released the login before that check and
  another account took it, the action stops as retryable (`login_changed`)
  and changes nothing for the new holder. GitHub has no user-ID-addressed
  organization membership mutation, so a rename between that last re-read
  and the call can still reach the new holder; if GitHub accepts it, the
  action may be reported as applied and not retried. Only a detected
  mismatch returns the retryable `login_changed`, and its retry starts from
  the bound ID. A rename refreshes the link's login.
- Team membership reads and changes address the team by the connection's
  immutable organization ID and the mapping's numeric team ID
  (`/organizations/{org_id}/team/{team_id}/memberships/{username}`), never
  by its slug. Renaming the mapped team and letting another team take its
  old slug therefore cannot add or remove anyone on the other team. A mapped
  team ID that no longer exists is a mapping error (`team_not_synced`,
  retryable): the team is never looked up by slug, so an admin fixes the
  mapping or the next sync creates a `create` team.
- **Team mappings** support both modes. `create` gives the Orglane team a
  GitHub team named after its slug and marked in the description with the
  instance ID; Orglane never takes over a team of that name that lacks its mark.
  `existing` uses the GitHub team the admin picks by numeric ID. Additions are
  always role `member`: no one becomes an organization owner or team
  maintainer. People already in the team who are not Orglane's stay.
- A person who is not in the organization is invited before a team addition;
  GitHub would otherwise invite them through the team without Orglane's proof.
- People lose GitHub access through the removal guard like every provider:
  a pending invitation is canceled, an active membership is removed (which also
  ends the team memberships), and a large removal waits for an admin's
  confirmation. Organization owners (`owner_protected`), billing managers
  (`role_protected`) and team maintainers (`maintainer_protected`) are never
  removed; team removals only target the instance's mapped teams. Deleting a
  mapping stops managing its team (its members stay) and cancels any pending or
  held removal from that team.
- **Removal confirmation.** GitHub answers the
  [membership](https://docs.github.com/en/rest/orgs/members#remove-organization-membership-for-a-user),
  team-membership and invitation cancellations with `204` and no resulting
  state, and it may process the change asynchronously: the following read can
  still serve the old membership (GitHub documents `202` "being processed
  asynchronously" for
  [accepting an invitation](https://docs.github.com/en/rest/orgs/members#update-an-organization-membership-for-the-authenticated-user)).
  Orglane therefore re-reads every removal and keeps it pending with the
  retryable `removal_pending` (a 5 s check interval) until GitHub confirms it;
  only the confirmed absence counts as `changed`, so the removal guard, the
  audit entry and the person's result do not report the removal early.
  A `200` whose
  [`direct_membership`](https://docs.github.com/en/rest/orgs/members#get-organization-membership-for-a-user)
  is false is a membership granted only by an enterprise team; GitHub documents
  that removing the direct membership leaves it in place, so Orglane reports
  the permanent `indirect_membership` and an admin removes the enterprise team
  membership instead.

## Minimal App permissions

Checked against GitHub's official documentation on 2026-11-05. These are the
minimum **M2 contract** permissions requested by both setup paths. Documentary
verification is distinct from real-GitHub acceptance: until the dedicated
`github-live` resources are supplied and tested, this is not a claim of a
successful live invite/remove or webhook round trip.

| Scope | Permission | Access | Why |
|---|---|---|---|
| Organization | Members | Read and write | [Invitations and membership removal](https://docs.github.com/en/rest/orgs/members), [team creation](https://docs.github.com/en/rest/teams/teams), and [team membership](https://docs.github.com/en/rest/teams/members) require Members write (#65). |
| Repository | Metadata | Read | Mandatory for GitHub Apps; identifies repositories visible to the installation. See [choosing permissions](https://docs.github.com/en/apps/creating-github-apps/setting-up-a-github-app/choosing-permissions-for-a-github-app). |
| Repository | Pull requests | Read | `pull_request`, `pull_request_review` and `pull_request_review_comment` subscriptions require Pull requests read (#66). See [webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads). |
| Repository | Issues | Read | `issue_comment`, including comments on pull requests for mentions, and `issues` (mentions in new issues) require Issues read (#66). See [webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads#issue_comment). |

Subscribe to `pull_request`, `pull_request_review`, `issue_comment`,
`pull_request_review_comment`, `issues`, `member`, `membership` and
`organization`. Apps created before #66 lack `issues`; add it under the App's
**Permissions & events** → **Subscribe to events** (no new permission, so no
re-approval by the organization). GitHub sends `installation`, `meta` and
`github_app_authorization` to every App without a subscription.
Organization member and team events use Members read, included in Members write.
No Contents, Administration, Actions, Checks or repository write permission is
requested. An installation may be limited to selected repositories; events and
reads outside that selection are unavailable.

[The manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
supplies the initial App private key, Client secret and webhook secret. The
manifest contains the requested permissions and events; the client never sends
those as trusted configuration to Orglane.

## Tokens and key rotation

App JWTs use RSA SHA-256, backdate `iat` by 60 seconds for clock skew, and expire
nine minutes from the current time. Private keys must be RSA PEM with at least
2048 bits. Requests go only to `api.github.com`, have bounded responses, reject
all redirects, and return safe errors without response bodies or request URLs.

Installation tokens are minted on demand with only the required permissions,
validated for expiry and held in a bounded memory cache keyed by instance,
installation, App ID and private-key digest. They refresh five minutes before
expiry, are never persisted, and disappear on restart. Before each mint, the
adapter re-verifies the installation and organization. See GitHub's
[installation token documentation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

Generate a replacement key in the App's GitHub settings, then use **Replace App
private key** in the instance's credentials card. The public
`PUT /api/v1/apps/instances/{instanceId}/credentials/app_private_key` endpoint
stages it. The existing rotation worker verifies it with `GET /app` before
activation; the previous key remains active until acknowledgement and retires
with an hour of overlap. Only revoke the previous key on GitHub after Orglane
reports successful verification. GitHub controls actual key revocation; keeping
an encrypted retiring key does not revoke it on GitHub.

Webhook-secret replacement uses the existing **Rotate** control and
`PATCH /app/hook/config`. The initial value comes from GitHub; replacements
are generated and sealed once by the core (`CredentialSpec.GenerateRotation`).
Timeouts, network failures and 5xx responses stay `uncertain`, and retries apply
the same value. GitHub's primary and secondary rate limits (403 or 429 with
`Retry-After`, `X-RateLimit-Remaining: 0` and `X-RateLimit-Reset`, or a
rate-limit message) are never taken as a rejection: the job waits until the
indicated time (one minute without one, at most an hour), on every GitHub call,
and the connect flow answers 429 with `Retry-After`. A definite rejection of the first attempt discards the
candidate; after an attempt whose outcome is unknown, a rejection proves nothing
about that attempt, so the candidate stays `uncertain` (and keeps verifying
deliveries) until GitHub acknowledges it or an admin abandons it. **Abandon**
first re-applies the active value, once Orglane's own earlier call to GitHub has
ended (at most a few minutes after an unanswered or interrupted call). No
upload, reconnect or direct store can overwrite a pending or uncertain
candidate. If neither retrying nor cancelling can finish, an admin can record
which value GitHub uses (**The provider's answer cannot be established**);
Orglane does not verify that choice, asks for a confirmation first, audits it
and shows it as an admin's choice, never as a verification: keep the old key on
GitHub until Orglane's calls with the new one succeed. #66 supplies incoming
proof-by-use webhook verification and drift detection (a delivery signed with
an older webhook secret is reported on the instance; rotate the secret then)
for GitHub.
Client-secret rotation is outside M2.

## Webhooks (#66)

The App carries one webhook, created by the manifest with the URL
`https://<workspace>/api/v1/webhooks/<instanceId>`; Orglane creates no
per-organization hooks. Each delivery is verified before anything is stored:
`X-Hub-Signature-256` must be `sha256=` and the hex HMAC-SHA256 of the raw body
under the App's webhook secret (during a rotation, under any version GitHub may
still use), compared in constant time. A failed check answers `401` and stores
nothing. Deliveries are deduplicated by the signed bytes and by
`X-GitHub-Delivery`; GitHub's redeliveries of the same bytes are duplicates.
Delivery rows expire after 30 days, but each signed body's *effects* (the
connection state changes below and the provisioning resync) are remembered for
the instance's lifetime in `webhook_effect_marks`. Replaying an old delivery
after that therefore stores a new delivery but never suspends, disconnects,
restores or resyncs again (`ignored` with `effect_replayed` when it has no
events); a captured suspension cannot degrade a healthy connection later, and
an old delivery cannot prove a deleted webhook works again.

Only signed body fields decide whether a delivery is for the instance: its
`installation.id` must be the connected installation and `organization.id`,
when present, the connected organization. Anything else is ignored
(`foreign_installation`; `no_installation` for App-level events other than
`meta` and `github_app_authorization`).

What Orglane stores (as normalized events, delivered to people from M3):

| GitHub event | Actions | Event |
|---|---|---|
| `pull_request` | `opened` | `code.pull_request.opened` (requested reviewers, assignees), and `code.mention.created` for the description |
| `pull_request` | `review_requested` | `code.review.requested` (a person, or a team without targets) |
| `issues` | `opened` | `code.mention.created` for the description |
| `issue_comment`, `pull_request_review_comment` | `created` | `code.mention.created` |
| `pull_request_review` | `submitted` | `code.mention.created` for the review's text |
| `organization` | `member_added`, `member_removed`, `member_invited` | `code.membership.changed` |
| `membership` | `added`, `removed` (team scope) | `code.membership.changed` |
| `member` | `added`, `removed` (repository collaborators) | `code.membership.changed` |

Edited comments and other actions create no events. Pushes are not received:
they would need the Contents permission, which M2 does not request.

Deliveries also change the connection:

- `installation` `deleted`: the instance becomes **disconnected**
  (`installation_deleted`); connect GitHub again to resume.
- `installation` `suspend`: **degraded** (`installation_suspended`) until
  `unsuspend` arrives.
- `meta` `deleted` (the App's webhook was deleted): **degraded**
  (`webhook_deleted`). Set the webhook URL and secret again in the App's
  settings; the next delivery for the installation restores the instance.
- `organization` member and `membership` team changes made in GitHub queue a
  drift check of provisioning, so desired and actual access are compared
  again (removals still go through the removal guard).

To check deliveries, open the App's **Advanced** → **Recent Deliveries** in
GitHub; Orglane's answer is `202` for a new delivery, `200` for a duplicate,
`401` for a bad signature and `404` for an instance that does not take
webhooks.

## Dedicated live-test setup (#71)

Only the maintainer supplies a dedicated test organization, test App and bot.
Never point these tests at an organization containing real code or people.
The full shared environment checklist is in
[providers.md](specs/providers.md#github-live-tests); #71 owns the workflow.

The shared harness requires all nine settings below. #64 uses the App and
installation values for connection checks, #65's provisioning test uses the bot
for invitation, team and removal checks, and the janitor also verifies the bot
identity. The webhook secret remains reserved for #66.

| Environment kind | Name | Value |
|---|---|---|
| Variable | `ORGLANE_TEST_GITHUB_ORG` | Dedicated test organization login |
| Variable | `ORGLANE_TEST_GITHUB_APP_ID` | Installed test App ID |
| Variable | `ORGLANE_TEST_GITHUB_APP_CLIENT_ID` | That App's Client ID |
| Variable | `ORGLANE_TEST_GITHUB_INSTALLATION_ID` | Installation on that test organization |
| Secret | `ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY` | That App's private key PEM |
| Variable | `ORGLANE_TEST_GITHUB_USER_LOGIN` | Dedicated non-owner bot login |
| Variable | `ORGLANE_TEST_GITHUB_USER_ID` | Bot's immutable user ID |
| Secret | `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` | Test App's webhook secret (#66) |
| Secret | `ORGLANE_TEST_GITHUB_USER_TOKEN` | Classic token belonging to that bot (see the token note below) |

Create the test App **inside the organization**: organization **Settings →
Developer settings → GitHub Apps**. An App created on a personal account with
**Only on this account** cannot be installed on the organization; transfer it
with the App's **Advanced → Transfer ownership** page instead. The bot must be
an ordinary organization **Member**, and the maintainer's own account the
**Owner**. If the organization was created from the bot account, invite the
maintainer as an Owner first and demote the bot before the live tests.

The test App's **webhook URL can be any valid https URL**, even one nobody
serves: the live checks read the App's delivery log, and GitHub cannot reach a
CI runner. Generate a delivery by opening an issue or adding a comment in a
sandbox repository in the test organization; if the App installation uses
selected repositories, select that sandbox repository first or the event creates
no App delivery. The real-signature check remains the manual check in the
[provider specification](specs/providers.md#github-live-tests).

`ORGLANE_TEST_GITHUB_USER_TOKEN` must be a **classic personal access token owned
by the bot**. The provisioning test removes the bot from the organization and
then accepts the re-invitation with `PATCH /user/memberships/orgs/{org}`. A
fine-grained token is tied to a resource owner and becomes inactive when the
user loses access to that resource, so an organization-owned fine-grained token
is expected to stop working once the bot is removed; a classic token is tied
only to the user and survives the re-invitation. The maintainer used a classic
token with scope `admin:org`. GitHub documents `write:org` as the narrower
scope for organization membership read/write; it is the minimum expected to
cover `PATCH /user/memberships/orgs/{org}` and should replace `admin:org` once
a live run confirms it. Do not add repository scopes for the current harness:
it creates no repository, pull request or comment. A future test that creates
issues, pull requests or comments needs an explicitly selected disposable
repository and only the repository permissions that test uses; never select
every repository in the organization.

The connection test itself makes no membership or repository mutations. Configure an environment named `github-live`, restrict deployment
branches to exactly `main` under the current maintainer policy (the pre-merge
and bootstrap procedures are in the [live test operations guide](github-live.md)),
and never expose it to untrusted PR code. The workflow maps secret/variable
names to the same process environment names and serializes runs
(`concurrency: github-live`).

Run the shared target, which also runs the M2 acceptance scenario's GitHub
part through the core's provisioning pipeline:

```sh
make test-github-live
# equivalent to:
go test -tags integration -count=1 -timeout 10m -v -skip '^TestGitHubLiveJanitor$' ./providers/github
go test -tags integration -count=1 -timeout 10m -v -run '^TestM2AcceptanceGitHubLive$' ./core/internal/apps
```

Locally and on PRs, missing inputs clearly skip the live test. In the protected
workflow, `ORGLANE_REQUIRE_GITHUB_LIVE=1` makes any missing required value fail.
The connection test verifies real App identity, the expected organization and
installation, permission levels, token issuance and an authenticated repository
listing. The provisioning test invites the dedicated non-owner bot by user ID,
creates one temporarily named team, adds the bot to it, removes it again, removes
the bot from the organization, cancels an invitation and restores the bot's
membership; it also re-runs the invitation ensure and requires GitHub to
attribute the invitation to the App's bot (`<app-slug>[bot]`). Tests use real
GitHub with no API mocks; temporary teams follow the naming convention above and
are deleted by the test. Post-removal reads wait up to 60 seconds for GitHub to
finish a queued removal or cancellation and, on timeout, report the last HTTP
status plus the non-secret membership `state`, `role`, `direct_membership` and
enterprise-team fields. Cleanup waits for an accepted removal to finish before
restoring the bot, so a removal that lands late cannot undo the restoration.
The test also repairs the bot at startup if a previous run's late removal left
it out of the organization, and waits for GitHub's member list to reflect the
restored membership.
The workflow runs the same two commands as
`make test-github-live` (provider live tests without the janitor, then the M2
acceptance test), followed by the janitor as a separate final step; see the
[live test operations guide](github-live.md#workflow-steps).

The M2 acceptance scenario adds the core's side of the same live resources:
`TestM2AcceptanceGitHubLive` (in `core/internal/apps`, build tag `integration`)
connects the App through the provisioning pipeline, adds the verified bot
account to a mapped team, verifies the membership with the real API and
deletes the temporary team. It creates no repository and does not open pull
requests or comments; those need an explicitly selected sandbox repository and
a bot token with write access to it, which the current classic `admin:org`
token deliberately does not grant (see below). Without the nine settings the test
skips with a message naming them, and `ORGLANE_REQUIRE_GITHUB_LIVE=1` turns
that into a failure.

External acceptance remains incomplete until that test passes with supplied
resources. Before release, also run the guided manifest/create/install browser
round trip and the documented manual fallback against that test organization.
The OAuth browser authorization/code-exchange leg for employee linking remains
a separate manual pre-release check (#95); preparing a redirect is not evidence
that OAuth completed. Real webhook signatures and event semantics are #66.

## Live-test resource ownership and janitor

#65/#66 must name every temporary team/repository
`orglane-test-<GITHUB_RUN_ID>-<GITHUB_RUN_ATTEMPT>-<suffix>` and set its description
to exactly `orglane-test:<GITHUB_REPOSITORY>:<run id>:<attempt>`. Normal cleanup
still belongs to the test that created it. The always-after-tests janitor is
invoked separately, and merely listing its entrypoint performs no API calls:

```sh
go test -tags integration -list '^TestGitHubLiveJanitor$' ./providers/github
go test -tags integration -count=1 -timeout 5m -v -run '^TestGitHubLiveJanitor$' ./providers/github
```

The janitor requires the nine shared settings plus `GITHUB_REPOSITORY` and the
ephemeral workflow `GITHUB_TOKEN` with `actions:read`. Regular tests do not need
that workflow token. It verifies the App, expected organization/installation,
required App permissions and dedicated bot identity before considering deletion.
It reads the exact originating workflow attempt through
`GET /repos/{GITHUB_REPOSITORY}/actions/runs/{run id}/attempts/{attempt}` and
requires a matching repository/run/attempt, `status: completed`, and both
`run_started_at` and `updated_at` strictly older than 24 hours, with the update
time no earlier than the start. `updated_at` supplies proof of inactivity after
completion. A long-running attempt or an old-start attempt that completed
recently is preserved. Unknown completion or update time fails closed. Names
alone, descriptions alone and timestamps from another run never authorize
cleanup. Repositories additionally require
their own creation timestamp older than 24 hours; teams without `created_at`
use the verified completed attempt and inactivity time. Unknown ownership or
age is preserved.

Only matching owned repositories and leaf teams are considered. The janitor
re-reads resource identity and ownership immediately before deletion, uses stable
numeric team IDs, and preserves parent teams to avoid cascading deletion of
unknown child teams. It never deletes users or memberships. Authentication,
listing, age-lookup and deletion errors fail the job. Once an owned resource is
eligible, even a 404 during re-read or deletion fails clearly because GitHub can
also use 404 to hide missing authorization; it is never reported as successful
cleanup. Missing originating attempts are preserved as
unverifiable, rather than guessed old.

Team cleanup uses the App's existing Members write permission. Repository
cleanup, when an owned repository older than a day exists, uses the existing
`ORGLANE_TEST_GITHUB_USER_TOKEN`, never a broader App permission. GitHub
requires repository admin access and either repository **Administration:
write** (fine-grained token, selected repositories) or the classic `delete_repo`
scope for [deleting a repository](https://docs.github.com/en/rest/repos/repos#delete-a-repository).
The current harness creates no repositories, so this path is not exercised, and
the verified classic `admin:org` token deliberately has no repository deletion
scope. Do not broaden it to all repositories or add `delete_repo` without a
maintainer decision; a future test that needs repository cleanup must bring an
explicitly selected disposable repository and a separately chosen, minimal
capability: a separate fine-grained janitor credential with Administration:
write on those repositories (and the workflow change to pass it), or a
maintainer-approved classic `delete_repo` scope on the bot token. This is
conditional test-only access: it is not a product App
permission, is not needed by #64's connection check, and is not required when
the janitor finds no eligible repository to delete.

If the bot cannot delete an eligible repository it can see, the janitor fails
clearly and preserves the external acceptance failure. A successful janitor run
proves only that no visible eligible resource remained; it cannot see or delete
repositories outside the token's access, so a maintainer who exercises
repository cleanup must inventory the organization separately. A
selected-repository fine-grained token may not see newly created repositories
until the maintainer explicitly selects them. #66's webhook live test creates no
repositories or teams: it reads
the App's delivery log, so it needs no repository cleanup; a test that opens pull
requests or comments to trigger deliveries still has to bring a selected
disposable repository, and must not solve it by broadening the token to all
repositories. #64 creates no test teams or repositories, so a
successful empty janitor run does not prove repository deletion is authorized.

### Rotation status and recovery

Instance details expose `credentialRotations`: the latest safe lifecycle outcome
per credential (`accepted`, `verifying`, `failed`, `verified`, `cancelling`, or
`cancelled`). These contain only name, version and outcome, never key values or
GitHub diagnostics. Terminal outcomes come from the audit log and remain visible
after discarded ciphertext is cleaned up. Upload acceptance is not verification.
A rejected replacement leaves the working key active; generate a PEM for the
correct App, check its App ID and installation, and retry. Verification only
reads from GitHub, so a rejection is final even after an earlier attempt failed
without an answer. Revoke the old GitHub
key only after the replacement is verified and active.

### Janitor provenance boundary

Cleanup also requires exact originating workflow path
`.github/workflows/github-live.yml` (bare or with the explicit `@main` ref),
repository and head repository
`orglane/orglane`, and a trusted event on `main` (`push`, `schedule`, or
`workflow_dispatch`). Missing/foreign metadata, PR events, and the current run
(including other attempts) never authorize cleanup. The current protected live
environment permits MAIN only. Non-main manual runs are preserved: their branch
name and `workflow_dispatch` event cannot prove the separate, frozen-SHA
maintainer authorization described in the live workflow operations contract.
This conservative boundary does not infer authorization from a test marker or
broaden environment policy. The completed attempt's last update and resource
creation (where available) must still be older than 24 hours; unknown provenance
or age is preserved. No real-GitHub acceptance is possible until a dedicated test
organization, App, bot and all nine documented settings are supplied.
