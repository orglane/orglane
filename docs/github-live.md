# GitHub live test operations

The [provider specification](specs/providers.md#github-live-tests) defines the
real-service acceptance checks for issues #64–#67. The
`.github/workflows/github-live.yml` workflow and its invocation checks belong to
#71; the provider harness (`providers/github`), `TestM2AcceptanceGitHubLive` in
`core/internal/apps` and the janitor belong to #64/#67. A green run requires
all of them. An absent provider package, missing test entrypoint, missing
configuration, failed provider or acceptance test, or janitor error fails the
run. Local and ordinary PR runs use the tests' documented skip behavior when the
configuration is absent.

## Maintainer setup

1. Create a **dedicated test organization** containing no real code or people
   (placeholder `orglane-test`), a test GitHub App (placeholder `Orglane CI`),
   and a dedicated bot account. Agents do not create these resources.

2. Create the App **inside the test organization**: organization **Settings →
   Developer settings → GitHub Apps → New GitHub App**. An App created on a
   personal account with **Only on this account** cannot be installed on the
   organization. If the App already exists on the wrong account, transfer it
   from the App's **Advanced → Transfer ownership** page; do not try to install
   a personal-only App on the organization. Install the App only on the test
   organization with the permissions and events verified in
   [docs/github.md](github.md). The janitor uses the App's existing Members
   write permission; do not broaden App permissions for it.

3. Make the bot an ordinary organization **Member**, never an Owner, and the
   maintainer's own account the organization **Owner**. If the organization was
   created from the bot account, first invite the maintainer's account as an
   Owner and then demote the bot to Member. The provisioning test removes the
   bot from the organization and restores it, and an owner can never be removed
   by that test.

4. The App's **webhook URL can be any valid https URL**, even one nobody serves.
   GitHub attempts a delivery for every event and cannot reach a CI runner, so
   use an intentionally unreachable or controlled URL: a served URL receives
   the test payloads. The live checks read the App's delivery log
   (`GET /app/hook/deliveries`) instead. `TestGitHubLiveWebhookDeliveries`
   verifies the logged `X-Hub-Signature-256` against the logged payload bytes
   with the App's real secret. To produce deliveries for the webhook checks,
   open an issue or add a comment
   in a sandbox repository in the test organization; the App's delivery log
   records the event. If the App installation uses selected repositories,
   select that sandbox repository first: an event in an unselected repository
   produces no App delivery. `TestGitHubLiveWebhookDeliveries` skips locally
   when the log has no handled delivery, but fails a required run with
   `ORGLANE_REQUIRE_GITHUB_LIVE=1`; generate an event before dispatching. Path
   (a) (the delivery log) is in use, so `gh webhook forward` is not needed.

5. Create the bot token. `ORGLANE_TEST_GITHUB_USER_TOKEN` must be a **classic
   personal access token owned by the bot**, not a fine-grained token. The
   provisioning test removes the bot from the organization and then accepts the
   re-invitation with `PATCH /user/memberships/orgs/{org}`. GitHub documents
   that both token types are tied to the user, but a fine-grained token is also
   limited to a resource owner and becomes inactive when the user loses access
   to that resource. An organization-owned fine-grained token is therefore
   expected to stop working once the bot is removed (and creating one requires
   an organization owner to approve it). A classic token is tied only to the
   user account, so it keeps working across removal and re-invitation.

   The maintainer used a classic token with scope `admin:org`. GitHub's
   documented narrower scope for organization membership read/write is
   `write:org`, and `PATCH /user/memberships/orgs/{org}` is listed under Members
   write for fine-grained tokens and under organization membership for classic
   scopes. Keep the maintainer's configured `admin:org`; switch to `write:org`
   only after a live run with that narrower scope confirms it. Do not
   add repository scopes for the current harness: it creates no repository,
   pull request or comment. A future test that opens pull requests or comments
   must bring an explicitly selected disposable repository and only the
   repository permissions it needs (see the [janitor requirements](github.md#live-test-resource-ownership-and-janitor));
   never select every repository in the organization for convenience.

6. In **orglane/orglane → Settings → Environments → github-live**, use selected
   deployment branches. The current maintainer policy is exactly one rule:
   branch `main`. Do not allow tags, wildcards, all protected branches, forks or
   ordinary PR refs. No human environment reviewer is required by the current
   maintainer policy; branch restrictions remain required. Store these six
   variables and three secrets **in this environment**, not at repository or
   organization scope:

   | Kind | Name | Required content |
   |---|---|---|
   | Variable | `ORGLANE_TEST_GITHUB_ORG` | Dedicated test organization's login |
   | Variable | `ORGLANE_TEST_GITHUB_APP_ID` | Test App ID |
   | Variable | `ORGLANE_TEST_GITHUB_APP_CLIENT_ID` | Test App client ID |
   | Variable | `ORGLANE_TEST_GITHUB_INSTALLATION_ID` | Installation on that organization |
   | Variable | `ORGLANE_TEST_GITHUB_USER_LOGIN` | Dedicated non-owner bot's login |
   | Variable | `ORGLANE_TEST_GITHUB_USER_ID` | That bot's numeric user ID |
   | Secret | `ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY` | App private key, PEM |
   | Secret | `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` | App webhook secret |
   | Secret | `ORGLANE_TEST_GITHUB_USER_TOKEN` | Bot's classic token with `admin:org` (or the minimal `write:org` once verified) |

7. Inspect **names only** when checking configuration. Do not print values,
   paste credentials into issues or logs, or overwrite existing secrets:

   ```sh
   gh api repos/orglane/orglane/environments/github-live \
     --jq '{name,protection_rules,deployment_branch_policy}'
   gh api repos/orglane/orglane/environments/github-live/deployment-branch-policies \
     --jq '.branch_policies[] | {name,type}'
   gh api repos/orglane/orglane/environments/github-live/variables \
     --jq '{total_count,names:[.variables[].name]}'
   gh api repos/orglane/orglane/environments/github-live/secrets \
     --jq '{total_count,names:[.secrets[].name]}'
   ```

## Triggers and pre-merge acceptance

Every push to `main` (including docs-only pushes) and the nightly run at 03:23
UTC requires the full configuration. The workflow maps the nine names above
unchanged and sets `ORGLANE_REQUIRE_GITHUB_LIVE=1`; it never converts missing
configuration into a successful skip. All runs share concurrency group
`github-live`, with cancellation disabled so superseding a run does not
interrupt resource cleanup. The live job's `GITHUB_TOKEN` has only
`contents: read` and `actions: read`. That permission applies to the whole job:
actions such as checkout can use the default token even though only the janitor
step explicitly maps `GITHUB_TOKEN` for run lookup. Review every action in the
live job against those permissions, not only the step that names the token.

The workflow has no PR trigger and never uses `pull_request_target`. Its
unprivileged first job rejects fork repository copies, PR events, tags and
non-main automatic runs before requesting the protected environment. The live
job checks out the event's immutable `github.sha`, does not persist Git
credentials, and exposes test secrets only to the test and janitor steps.

A manual branch run executes that branch's code with secrets. Treat granting an
environment branch rule as granting credential access. For pre-merge
acceptance, the coordinator/maintainer must:

1. Obtain independent Codex and CodeRabbit review, resolve review threads, and
   confirm green CI for the **full head SHA**, including changes brought from
   #64/#67 and the workflow itself. Never run a fork's or unreviewed PR's code
   with these secrets.
2. Freeze the reviewed head of an exact same-repository acceptance branch and
   authorize that branch explicitly. Temporarily add **only its exact branch
   name**, preserving the `main` rule. Verify the branch still resolves to the
   reviewed SHA immediately before dispatch. No tags or wildcard exceptions.
3. Dispatch, then verify the recorded run's `head_sha` equals that SHA:

   ```sh
   gh workflow run github-live.yml --repo orglane/orglane --ref <reviewed-branch>
   gh run view <run-id> --repo orglane/orglane \
     --json headSha,event,status,conclusion,url
   ```

4. Remove the temporary branch rule after the run, including on failure.
   Record the run URL, full tested SHA, provider and janitor outcomes, and
   names-only configuration/protection evidence.

The janitor only accepts resources from a completed `main` run (see below).
Non-main manual runs are preserved, not deleted: run metadata cannot prove the
separate frozen-SHA maintainer authorization. If a manual-run test fails before
its own cleanup, the maintainer must remove those resources manually; the
janitor keeps preserving them until a future policy can prove the authorization.

### Bootstrap

GitHub registers `workflow_dispatch` only for workflows present on the default
branch. The workflow was merged to `main` before its first dispatch, and the
first `main` run passed
([run 42684360151](https://github.com/orglane/orglane/actions/runs/42684360151)),
so `gh workflow run github-live.yml` works from `main`. A pre-merge run uses the
temporary exact-branch rule described above: the workflow is already on `main`,
so add only the exact acceptance branch to the environment's deployment branch
policies, dispatch with `--ref <branch>`, verify `head_sha`, and remove the rule.
The janitor preserves non-main manual resources, so leftovers need manual
removal. Never use a wildcard, tag, fork or `pull_request`/`pull_request_target`
trigger, and never disable the unprivileged trust barrier to work around a
dispatch limitation. Main acceptance additionally requires a green required run
on `main` with the maintainer's configuration.

## Workflow steps

The `tests` step runs the same commands as the maintainer's local target:

```sh
go test -tags integration -count=1 -timeout 10m -v -skip '^TestGitHubLiveJanitor$' ./providers/github
go test -tags integration -count=1 -timeout 10m -v -run '^TestM2AcceptanceGitHubLive$' ./core/internal/apps
```

Before any test can mutate the organization, the wrapper checks that both exact
entrypoints exist: `TestGitHubLiveJanitor` in `./providers/github` and
`TestM2AcceptanceGitHubLive` in `./core/internal/apps`. `go test -run` succeeds
when it matches nothing, so an empty match must never count as a successful
live test. Listing compiles the packages but executes no tests and makes no
GitHub API calls.

The separate final `janitor` step runs only `TestGitHubLiveJanitor`, with the
ephemeral workflow `GITHUB_TOKEN`, after the test step was reached. It runs even
when the tests failed. The wrapper requires all nine values and
`ORGLANE_REQUIRE_GITHUB_LIVE=1`; missing configuration fails before Go runs.

The M2 acceptance test creates its temporary team through the core's
provisioning pipeline. The provider sets that team's description to
`Managed by Orglane (<instance id>)`, not the exact `orglane-test:...` marker
the janitor requires. If the M2 test stops before its own cleanup, the janitor
preserves the team. Delete `orglane-test-<run id>-<attempt>-m2` manually in the
test organization, after confirming its run marker and using the immutable team
ID, until the ownership contracts are reconciled. Never delete a similarly
named team you cannot attribute to the run.

## Cleanup contract (#64)

The detailed ownership, age and provenance rules are in
[docs/github.md](github.md#live-test-resource-ownership-and-janitor). In short:

- Validate the configured App, installation and expected dedicated test
  organization, and the bot identity, before listing anything.
- Consider only teams and repositories in that organization whose name is
  exactly `orglane-test-<run id>-<attempt>-<suffix>` and whose description is
  exactly `orglane-test:<GITHUB_REPOSITORY>:<run id>:<attempt>`. The provider
  provisioning test temporarily uses `orglane-test:<repository>:<run id>-<attempt>`
  (a hyphen between run and attempt), and the M2 team uses `Managed by Orglane
  (<instance id>)`; neither matches the janitor marker, so check for those
  leftovers manually after a failed run and delete them by immutable team ID
  and verified run marker.
- Prove the originating completed workflow attempt through
  `GET /repos/orglane/orglane/actions/runs/{run_id}/attempts/{attempt}`: matching
  run and attempt, repository and head repository `orglane/orglane`, workflow
  `.github/workflows/github-live.yml`, and a trusted `push`, `schedule` or
  `workflow_dispatch` event on `main`, excluding the current run. Require the
  run start and last update (and a repository's creation time) to be strictly
  older than 24 hours, and preserve anything whose age or ownership cannot be
  verified.
- Non-main manual runs are preserved: a branch name and `workflow_dispatch`
  event cannot prove the separate frozen-SHA maintainer authorization. This is
  deliberately conservative and does not infer authorization from a test marker.
- Never delete accounts or organization memberships. Preserve parent teams so
  unknown children are not cascade-deleted. Fail clearly on authentication,
  listing, lookup or deletion errors instead of falling back to broad deletion.
- Use the App's Members write permission for teams. Repository deletion is not
  exercised by the current harness (no test creates a repository). If an
  eligible owned repository ever exists, a separately selected minimal
  capability is required: repository admin access and either a separate
  fine-grained janitor credential with Administration: write on those selected
  repositories (plus the workflow change to pass it) or a maintainer-approved
  classic `delete_repo` scope. It is not a product App permission and is not
  needed by the connection or acceptance tests.

The janitor contract and a real `main` run have passed
([run 42684360151](https://github.com/orglane/orglane/actions/runs/42684360151)).
The manual OAuth browser check remains the separate pre-release check in the
[specification](specs/providers.md#manual-pre-release-checks).
