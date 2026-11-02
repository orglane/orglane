# Security Policy

## Supported versions

Orglane is in pre-alpha. Only the latest commit on `main` and the latest release receive security fixes.

## Reporting a vulnerability

**Please do not report security vulnerabilities through issues, discussions or pull requests.**

How to report depends on the repository's visibility:

- **While the repository is private (pre-alpha):** only maintainers and collaborators can see it. Report vulnerabilities directly to the maintainers through a private channel: send a private message to a member of the `@orglane/maintainers` team.
- **Once the repository is public:** use GitHub private vulnerability reporting. Open the repository's **Security** tab and choose **Report a vulnerability**.

Please include:
- a description of the issue and its impact;
- steps to reproduce or a proof of concept;
- affected versions or commits.

We aim to acknowledge reports within a few days and will keep you informed while we work on a fix. We're happy to credit reporters in the release notes unless you prefer otherwise.

## Scope

In scope: the Orglane server, web UI, mobile apps, installer, official providers and the push relay.
Out of scope: vulnerabilities in third-party services themselves (Forgejo, GitHub, ...). Please report those upstream; tell us too if Orglane's integration makes them worse.
