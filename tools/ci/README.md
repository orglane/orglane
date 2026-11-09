# CI tooling

`go run ./tools/ci <command>`: the repository's CI logic, kept out of YAML so it can be tested. It uses only the Go standard library. Changes here are `risk/high` and need a maintainer ([ADR 0017](../../docs/adr/0017-development-process.md)).

| Command | Used by | What it does |
|---|---|---|
| `risk` | CI `changes` job | Classifies a change and reports whether code changed, so docs-only pull requests skip the heavy jobs |
| `policy` | `Policy` workflow, orchestrator | **Trusted:** computes the risk tier, applies exactly one `risk/*` label and reports the merge checklist; it does not publish a commit status |
| `licenses` | `make licenses` | Checks dependency licenses against the [ADR 0016](../../docs/adr/0016-licensing.md) policy |
| `release-version` | `Images` workflow (build) | Validates a release tag (`vX.Y.Z[-pre]`) and prints its image version |
| `release-aliases` | `Images` workflow (promote) | Lists the tags of the images released together (core and agent) and computes one plan from the versions published for all of them: where `latest` and every `X.Y` alias must point, see [docs/releasing.md](../../docs/releasing.md) |

### Trust model

CI (`.github/workflows/ci.yml`) runs on `pull_request`, so it executes the pull request's own workflows, local actions and `tools/ci`. A pull request could change any of them, so nothing CI computes decides the pull request's risk or eligibility. Its `changes` job only saves minutes by skipping jobs, and the policy rejects a skipped job when code changed.

The decision is made by `policy`, run from code the pull request cannot change:

- the **Policy workflow** (`.github/workflows/policy.yml`) runs on `workflow_run` (CI completed), `issue_comment` and `workflow_dispatch`. These triggers always use the default branch's workflow file. The job checks out the default branch, builds `tools/ci` from it, fetches the pull request's commits (`refs/pull/N/head`) **as data** and diffs them against the merge base. Nothing from the pull request is checked out or executed. It applies the `risk/*` label and writes the checklist to the job summary; it does not publish a commit status ([ADR 0020](../../docs/adr/0020-merge-checklist.md));
- the **orchestrator** runs the same checklist before merging, from an up-to-date checkout of `main`, because CI executes the pull request's own code:

  ```sh
  GITHUB_TOKEN="$(gh auth token)" GITHUB_REPOSITORY=orglane/orglane go run ./tools/ci policy -pr <number>
  ```

  It prints the tier and every requirement, and exits 0 only when the pull request is eligible. Merge only then.

A pull request that changes the policy itself (`.github/**`, `tools/ci/**`) is always `risk/high`, as judged by the policy on `main`.

## Risk tiers

Tier rules live in [`internal/risk/risk.go`](internal/risk/risk.go). The highest matching tier wins.

**High** (a maintainer must review):

- paths: `.github/**`, `tools/ci/**`, `Makefile`, `.golangci.yml`, `biome.json` (CI configuration), `release-please-config.json`, `.release-please-manifest.json` (releases are a human decision, so the release pull request needs a maintainer), `**/LICENSE`, `TRADEMARK.md`, `docs/adr/**`, `core/internal/identity/**`, `core/internal/permissions/**`, `core/internal/authz/**`, `core/internal/crypto/**`, `core/migrations/**`, `core/internal/agent/**`, `core/api/**`. Renames count for both the old and the new path. Keep this list in sync with `.github/CODEOWNERS`;
- weakened tests (the threat model is accidental weakening by agents, so the checks are conservative rather than a full parser):
  - **any modified or deleted line in an existing test file or fixture** (`*_test.go`, `*.test.*`, `*.spec.*`, `tests/`, `testdata/`, `__tests__/`, `__fixtures__/`). Counting cannot judge a changed matcher or argument (`toEqual({ id: 123 })` → `toBeDefined()`), so any change to existing tests needs a maintainer. Pure additions (new test files, new lines in existing test files) keep the normal tier. The checks below still run and say *what* changed in the summary;
  - a deleted test file, or **any rename or move** of a test file (the new name may exclude it, e.g. `_a_test.go`, `a_windows_test.go`, or a path outside the runner's include list);
  - an added or removed **Go build constraint** (`//go:build`, `// +build`) in a test file. The one exception is `//go:build integration` in a new file (CI runs that tag);
  - a change to **test discovery**: `vitest.config.*`, `vitest.workspace.*`, `vite.config.*`, or the `test`/`vitest*` entries of a `package.json`. `Makefile` and `.github/**` are high anyway;
  - an added executable skip marker (`t.Skip`, `.skip(`, `it.skip`, `describe.skip`, `test.skip`, `.only`, `xit(`, ...), including one added inside a comment or string;
  - an assertion (`t.Fatal`, `t.Error`, `require.`/`assert.`, `expect(`, ...) that no longer executes. Assertions are **counted over whole files**: every executable assertion in the old version of the changed test code must still execute in the new version (same code, whitespace ignored), so moving or re-indenting is fine. Go is lexed with `go/scanner`. TS/JS uses a small tokenizer that handles comments, strings, template literals (text is data, `${...}` substitutions are code, nested to any depth) and regex literals. Code inside comments or multi-line literals, and text inside strings, does not count, and neither do docs, fixtures or non-test code. A changed assertion counts as removed;
  - a changed test file that **cannot be analyzed confidently**. The tokenizer fails, and the change is high, instead of guessing: on a `/` after `}` (regex or division), on unterminated strings, comments, templates or regexes, on unbalanced brackets, and on JSX text it cannot read (such as `don't`).

Test files are `*_test.go`, `*.test.*` and `*.spec.*` (JS/TS), everything under `tests/`, and `testdata/` and `__tests__/` directories.

**Low** (auto-merge after green checks once enabled): every changed file is documentation (`docs/**` outside ADRs, `*.md` anywhere outside the high-risk paths), an added test (a new test file or fixture, or a test file that only gains lines) or an i18n catalog (`.json`, `.yaml`, `.po` under a `locales/` directory).

**Medium**: everything else.

When only documentation and license texts change (both paths of a rename), `code=false` and CI skips lint, tests, builds and the security scan.

## Merge checklist

`policy` reports **eligible** only when **all** of these hold. The checklist is advisory: no commit status is published, so a maintainer or the orchestrator must run it before merging.

1. The pull request comes from a branch of this repository. Pull requests from forks are never eligible.
2. The required CI checks (`changes`, `lint`, `test`, `build`, `security`) are reported for the head commit, and every check and commit status is green (`success`, `neutral`, or `skipped`). `skipped` is accepted only when the policy itself found that no code changed. The retired `merge-gate` status on older commits is ignored.
3. The risk tier is not high: the higher of the computed tier and any `risk/*` label (a maintainer can raise the tier with a label, never lower it). High reports **maintainer required**.
4. The latest `Codex review:` comment by a user with write access says `APPROVE` (or `APPROVED`) on its first line and **names the head commit SHA** (full, or at least 7 hex digits), for example:

   ```
   Codex review: APPROVE for 4f2c9d1e8a7b

   Reviewed commit: 4f2c9d1e8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d
   ```

   Commit dates are set by the author, so they cannot prove that a review saw the current code. Every new push needs a new approval that names it.
5. No review thread is unresolved.

The `Policy` workflow re-evaluates when CI completes and when a `Codex review:` comment is posted or edited. Run it on demand with `gh workflow run policy.yml -f pr=<number>` (for example after resolving a thread or changing a label).

Nothing enables auto-merge yet, so the checklist stays advisory: a maintainer runs it and merges the pull request. The `main` ruleset requires the CI checks themselves (`lint`, `test`, `build`, `security`).

## License policy

Policies live in [`internal/licenses/licenses.go`](internal/licenses/licenses.go):

- **server** (all dependencies, including dev tooling): permissive licenses (MIT, BSD-2/3-Clause, Apache-2.0, ISC, 0BSD, Python-2.0, ...), MPL-2.0, and copyleft licenses compatible with AGPL-3.0-or-later (LGPL, GPL-3.0, GPL-2.0-or-later, AGPL-3.0);
- **apache** (production dependencies of `packages/*`): permissive licenses and MPL-2.0.

SPDX expressions are evaluated (`OR`: one side allowed, `AND`: both). Unknown, missing or non-SPDX licenses fail the check. Go licenses come from `go-licenses report`, npm licenses from `pnpm licenses list`.

The `risk` command rejects files in reserved PR-review directories
(`.github/pr-assets/` and retired `docs/screenshots/`) in the requested head
commit, including on documentation-only PRs. `make lint-pr-assets` checks the
local git index, so force-added captures cannot bypass the ignore rules.
See [UI review evidence](../../CONTRIBUTING.md#ui-review-evidence).
