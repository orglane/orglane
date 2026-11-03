# Security Policy

## Supported versions

Orglane is in pre-alpha. Only the latest commit on `main` and the latest release receive security fixes.

## Reporting a vulnerability

**Please do not report security vulnerabilities through issues, discussions or pull requests.**

Report vulnerabilities privately through GitHub private vulnerability reporting: open the repository's [**Security** tab](https://github.com/orglane/orglane/security) and choose **Report a vulnerability** (or go straight to [the reporting form](https://github.com/orglane/orglane/security/advisories/new)). This is the only reporting channel; the report stays visible only to you and the maintainers until the advisory is published.

Please include:
- a description of the issue and its impact;
- steps to reproduce or a proof of concept;
- affected versions or commits.

We aim to acknowledge reports within a few days and will keep you informed while we work on a fix. We're happy to credit reporters in the release notes unless you prefer otherwise.

## Scope

In scope: the Orglane server, web UI, mobile apps, installer, official providers and the push relay.
Out of scope: vulnerabilities in third-party services themselves (Forgejo, GitHub, ...). Please report those upstream; tell us too if Orglane's integration makes them worse.
