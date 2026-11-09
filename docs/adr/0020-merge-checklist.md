# 0020. Merge checklist instead of a merge-gate commit status

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

[ADR 0017](0017-development-process.md) defines the risk tiers and the merge policy. The trusted `Policy` workflow implements it and reports the result as a `merge-gate` commit status on the pull request's head commit; the repository ruleset lists `merge-gate` as a required status check.

The status makes green CI look failed. For every `risk/high` pull request — every change to CI, the public API contract, identity, migrations or another high-risk path, and every modification or deletion in an existing test — it is always `failure` with the description "maintainer required". It duplicates the workflow's job summary and the `go run ./tools/ci policy` checklist, and because the `main` ruleset requires it, the always-failing check blocks high-risk pull requests even though a maintainer must merge them. Nothing enables auto-merge yet.

On 2026-11-09 the maintainer decided to remove the status. The `risk/*` labels and the policy checklist stay, and merging a `risk/high` pull request remains a maintainer decision.

## Decision

- The `Policy` workflow keeps computing the risk tier, applying exactly one `risk/*` label and writing the checklist to the job summary. It no longer creates or updates any commit status and no longer requests `statuses: write`.
- The `policy` command remains the merge checklist. `go run ./tools/ci policy -pr <n>` from an up-to-date `main` reports the same requirements as before: a same-repository branch, all required and other checks green, a tier below high, an AI `APPROVE` naming the head commit, and no unresolved review threads. Its exit status and report stay the same; it just does not publish a status.
- A maintainer removes `merge-gate` from the required status checks of the repository ruleset when this change lands. Repository settings are not changed from CI.
- Risk tiers, the `risk/high` maintainer requirement, the `Codex review:` approval format and the trust model (the `Policy` workflow runs default-branch code) stay as in ADR 0017. This ADR supersedes only the part of ADR 0017's implementation that published the merge decision as the `merge-gate` commit status.

## Consequences

- A `risk/high` pull request no longer shows a permanently failing required check; reviewers see the tier in the label and the checklist in the job summary.
- Merge eligibility is advisory: the orchestrator or a maintainer runs the checklist before merging. Nothing merges on its own until branch protection and auto-merge are enabled.
- The checklist no longer appears as a status on the head commit. Tools that read the `merge-gate` context must use the label, the job summary or the command instead; the policy ignores the retired status on older commits.
- If auto-merge is enabled later, it needs a deliberate gate, for example a required check that the policy can publish without making high-risk pull requests look failed. That decision is not made here.

## Alternatives considered

- **Keep the status and ignore it.** Rejected: a required check that always fails for high-risk changes makes green CI look failed and trains people to ignore red checks.
- **Publish `success` for `risk/high` too.** Rejected: the status would always be green and convey nothing; the label and the checklist already say that a maintainer is required.
- **Keep the status but remove it from the required checks.** Rejected: the status itself is the confusing part; an optional red mark on every high-risk commit does not help.
