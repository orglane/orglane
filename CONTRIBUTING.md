# Contributing to Orglane

Thanks for your interest in Orglane! The project is in **pre-alpha**. The architecture is being laid down and the codebase is young, so the most valuable contributions right now are feedback on the [brief](docs/brief.md), the [architecture](docs/architecture.md) and the [decisions](docs/adr/).

## Ways to contribute

- **Discuss:** ideas and questions go to GitHub Discussions.
- **Report:** bugs and concrete proposals go to Issues (please use the templates).
- **Build:** pick an issue labeled `good first issue` or `help wanted`, or propose a new provider using the provider request template.
- **Translate:** translations will be managed through Weblate once the UI exists.

## Workflow

1. For anything non-trivial, open or comment on an issue first, so we agree on the approach.
2. Fork the repository and create a branch.
3. Make focused changes with tests. Integration tests run against real services in containers, not mocks.
4. Use [Conventional Commits](https://www.conventionalcommits.org/) for the PR title (`feat:`, `fix:`, `docs:`, ...). PRs are squash-merged.
5. Open a pull request. Automated checks and an AI reviewer will respond quickly, and a maintainer reviews every external contribution.

### UI review evidence

PR-only screenshots and videos belong in GitHub PR descriptions or comments as
uploaded attachments. Generate locally under ignored `.github/pr-assets/` (web,
website, directory, or a feature subdirectory), then upload before deleting local
copies. Never commit review captures, including under the retired
`docs/screenshots/` directory; `make lint-pr-assets` checks the git index, including
force-added files. App icons and genuine product, marketing and documentation
assets remain in their appropriate source asset directories.

For visible changes, attach before/after views or a relevant screen × viewport ×
theme matrix using demo data. Record the exact reviewed commit SHA, browser and
version, viewport sizes, themes, commands/results, and verification limits
(including any untested browsers, scenarios or visual polish). A historical
capture must name its source SHA and must not claim to verify the current design.
Inspect captures for secrets and private real user data before uploading.

Use the GitHub UI or attachment-capable `gh pr comment` / `gh pr edit` with
`--body-file` and repeated `--attach ./safe-name.png` from a staging directory.
Reference each file as `![description](./safe-name.png)` in the body, then verify
the published Markdown contains hosted GitHub asset URLs and that every asset
loads. After a partial failure, inspect the published comment and retry only
missing files. Reuse hosted URLs when linking the evidence from other PRs;
keep source hashes and provenance in the PR, not image binaries in git.

### Review and risk tiers

PRs are labeled by risk based on the files they change (see [ADR 0017](docs/adr/0017-development-process.md)). Changes to identity, permissions, cryptography, database migrations, the agent, public API contracts, CI configuration or licenses always need a maintainer review, as do changes that remove or weaken tests. **External PRs are never merged automatically.**

### Architectural changes

Significant design changes need an ADR in `docs/adr/` (copy the [template](docs/adr/0000-template.md)). Accepted ADRs are not edited. A new ADR supersedes an old one.

## Licensing of contributions

By submitting a contribution, you agree that it is licensed under the license of the part of the repository it targets (AGPL-3.0-or-later for the server and web UI, Apache-2.0 for apps, SDKs and shared packages; see the `LICENSE` file in the relevant directory). You also grant the Orglane maintainers the right to relicense your contribution as part of the project.

You confirm that you have the right to submit the contribution.

## Code of conduct

Be respectful and constructive. A formal code of conduct will be added before the public launch.

## Security

Please do not report security vulnerabilities in issues, discussions or pull requests. Report them through GitHub's [private vulnerability reporting](https://github.com/orglane/orglane/security/advisories/new), the only reporting channel. See [SECURITY.md](SECURITY.md) for details.
