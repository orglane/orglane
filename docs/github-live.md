# GitHub live test operations

The [provider specification](specs/providers.md#github-live-tests) defines the
real-service acceptance checks for issues #64–#66. Issue #71 supplies
`.github/workflows/github-live.yml` and its invocation checks; the provider test
harness, including its janitor, belongs to #64. A green workflow requires both.
An absent provider package, missing janitor entrypoint, missing configuration,
failed provider test or janitor error fails the run. Local and ordinary PR runs
use the provider tests' documented skip behavior when configuration is absent.

## Maintainer setup

1. Create a **dedicated test organization** containing no real code or people
   (placeholder `orglane-test`), a test GitHub App (placeholder `Orglane CI`),
   and a dedicated bot account. Agents do not create these resources.
2. Install the App only on that test organization with the permissions and
   events verified by #64 in `docs/github.md`. Check the App ID, client ID and
   installation's organization against the intended resources. Do not broaden
   App permissions just for the janitor.
3. The bot must be a member, **never an organization owner**. Prepare its
   fine-grained token only for accepting invitations and creating the test PRs
   and comments that generate events, as specified by the provider tests.
   When repository cleanup is exercised, the same token may additionally have
   repository **Administration: write**, with the bot's admin role and token
   selection restricted to explicitly selected disposable repositories in this
   dedicated test organization. This test-only permission does not apply to
   the product App, production repositories, all repositories in the
   organization, or a classic `delete_repo` token. #64's connection smoke test
   creates no repositories and must not require this unused permission. This
   checklist does not authorize changing any real token or repository access.
4. In **orglane/orglane → Settings → Environments → github-live**, use selected
   deployment branches with exactly one initial rule: branch `main`. Do not
   allow tags, wildcards, all protected branches, forks or ordinary PR refs.
   No human environment reviewer is required by the current maintainer policy;
   branch restrictions remain required. Store these six variables and three
   secrets **in this environment**, not at repository or organization scope:

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
   | Secret | `ORGLANE_TEST_GITHUB_USER_TOKEN` | Bot's fine-grained token; selected disposable repository administration only when cleanup is exercised |

5. Inspect **names only** when checking configuration. Do not print values,
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
interrupt resource cleanup. The token has only `contents: read` and
`actions: read`; only the janitor step receives it explicitly for run lookup.

The workflow has no PR trigger and never uses `pull_request_target`. Its
unprivileged first job rejects fork repository copies, PR events, tags and
non-main automatic runs before requesting the protected environment. The
live job checks out the event's immutable `github.sha`, does not persist Git
credentials, and exposes test secrets only to the test and janitor steps.

A manual branch run executes that branch's code with secrets. Treat granting
an environment branch rule as granting credential access. For pre-merge
acceptance, the coordinator/maintainer must:

1. Obtain independent Codex and CodeRabbit review, resolve review threads, and
   confirm green CI for the **full head SHA**, including changes brought from
   #64 and the workflow itself. Never run a fork's or unreviewed PR's code with
   these secrets. The workflow file must first be registered on the default
   branch for GitHub to offer manual dispatch; a first rollout may therefore
   need a maintainer-approved bootstrap decision. Do not merge this workflow
   while its required real-test acceptance remains unmet.
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
   names-only configuration/protection evidence. Main acceptance additionally
   requires a green required run on `main` with the maintainer's configuration.

## Provider harness and cleanup interface (#64)

Both commands run with build tag `integration`, package `./providers/github`,
`-count=1` and bounded timeouts. The regular step runs the package's tests with
`TestGitHubLiveJanitor` excluded. The separate final step runs exactly
`TestGitHubLiveJanitor`, even after test failure or cancellation when the test
step was reached. The wrapper checks that this entrypoint exists before any
test execution; an empty match must not be accepted as successful cleanup.
Tests must fail with required mode enabled if configuration is incomplete.
Listing tests must not mutate GitHub resources.

The workflow passes GitHub's standard `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT` and
`GITHUB_REPOSITORY` context through the runner environment. Each test names
every created team and repository `orglane-test-<run id>-<attempt>-<suffix>` and
records the description marker
`orglane-test:<GITHUB_REPOSITORY>:<run id>:<attempt>` to prove ownership.
It deletes its resources through normal test cleanup and never creates or
deletes user accounts. The janitor is a recovery path for abandoned resources;
the wrapper does **not** implement GitHub API cleanup itself. #64's connection
test itself creates no organization resources.

The janitor contract agreed with #64 is:

- Validate the configured App, installation and expected dedicated test
  organization before listing or changing anything. Do not infer that an
  arbitrary organization is safe merely because a token can access it.
- Consider only teams and repositories in that organization with the exact
  test naming format and verifiable test ownership. Never delete unrelated
  resources, accounts or organization memberships.
- Delete only resources proven older than 24 hours. For teams without a
  creation timestamp, parse the originating run ID and attempt and look up
  `GET /repos/orglane/orglane/actions/runs/{run_id}/attempts/{attempt}`, using
  the ephemeral `GITHUB_TOKEN`. Verify the response ID and attempt match the
  resource name and marker, that the repository and head repository are
  `orglane/orglane`, and that its workflow is `.github/workflows/github-live.yml`.
  Verify a permitted event (`push`/`schedule` on `main`, or authorized manual
  dispatch), a completed run, and timestamps proving it has been inactive for
  more than a day. Never trust a timestamp encoded only in a resource name or
  use the candidate resource's organization as the run lookup repository.
- Preserve the current run, recent resources and any resource whose age,
  organization or ownership cannot be verified. Report verification/API and
  deletion errors as test failures, without falling back to broad deletion.
  Do not add a stored token or broaden the App permissions for run lookup.
- The accepted App Members permission allows team deletion, not repository
  deletion. Only after the organization, marker, run/attempt and age checks
  pass may the janitor use `ORGLANE_TEST_GITHUB_USER_TOKEN` for an eligible
  repository deletion, with the selected-repository Administration permission
  and bot admin role described above. If a verified eligible repository
  exists but that authority is unavailable, fail clearly; never fall back to
  the product App, workflow token or host `gh` credentials. Do not request this
  scope when no repository cleanup is exercised. If dynamic repository
  creation makes explicit token selection impractical, record that concrete
  #66 limitation instead of selecting every repository in the organization.

Issue #71 cannot claim janitor or external acceptance until #64 implements
this interface and a real run verifies it. The manual OAuth browser check and
any required raw webhook signature check remain the separate pre-release
checks in the [specification](specs/providers.md#github-live-tests).
