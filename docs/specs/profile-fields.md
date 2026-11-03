# Profile fields (issue #24)

Field definitions and values live in `core/internal/profile`. Fixed identity fields remain on the person. All profile reads and value updates use the same service policy; clients receive authorized entries and never decide which values to disclose.

## Decisions

- Reuse PostgreSQL, sqlc, the audit transaction helper, and the existing AES-256-GCM sealer. No new dependency. A separate HKDF purpose derives the profile key from the instance master secret; authenticated data binds each ciphertext to its person and field IDs.
- Definitions have immutable keys and types. Custom keys and option keys are derived from labels when omitted; library keys are reserved. Library labels are translated by key in the web catalog. Library option labels and field types remain fixed.
- Archive is reversible: values remain stored but neither values nor fields appear in profiles until restored. Section and position order the profile. The onboarding flag is stored for the onboarding flow; this issue does not implement that flow.
- Sensitive-read permission is required for every sensitive field read, including self. A viewer can edit only values they can read; sensitive values therefore also require that permission to edit. Changes that decrypt sensitive values (declassification or removing select choices) require the permission, and successful sensitive reads are audited atomically. Ordinary definition changes never reveal values.
- Visibility is cumulative with sensitivity: self and people managers can see ordinary fields of every visibility; other viewers see everyone fields or fields shared with an active team. A sensitive scope does not by itself bypass team/admin visibility. Hidden values remain visible to self and people managers, subject to the sensitive gate.
- A non-sensitive library birthday exposes the full date to self and sensitive readers; everyone else receives only `--MM-DD`. Custom dates retain their year. Marking birthday sensitive applies the sensitive gate first, even to self.
- Hidden values are omitted from the response while visible field metadata remains, allowing the profile layout to stay consistent. Only the person can hide or unhide a value. If an admin disables further hiding, the person can still show an already-hidden value again. Admins can still edit ordinary hidden values.
- Enabling a library field again restores its current settings, rather than overwriting admin choices. Removing select choices that have stored values returns a conflict; sensitive validation reads still commit their audit entries, with no definition or value writes. Sensitivity changes transform all existing values within the definition transaction.
- Team sharing is injected through `profile.Teams.SharesTeam`. The production stack supplies the identity service with `profile.WithTeams`; it grants visibility only through shared active teams between non-offboarded people. Integration coverage uses actual memberships, including removal, multiple shared teams, and archiving; an HTTP test verifies the production stack wiring.
- The web profile-fields card can be reused by the full directory detail view. Settings exposes library toggles, a three-step name/type/visibility flow, and collapsed advanced settings. Native controls fit the existing CSP and need no additional UI library.

## Access examples

These examples assume an active, non-hidden field unless the row says otherwise. The sensitive-admin column holds both `people.manage` and `people.sensitive.read`; the other admin holds only `people.manage`.

| Value | Self | Teammate | Other member | Admin | Sensitive admin |
|---|---|---|---|---|---|
| Everyone | Yes | Yes | Yes | Yes | Yes |
| Team | Yes | Yes | No | Yes | Yes |
| Admins | Yes | No | No | Yes | Yes |
| Sensitive, everyone | Only with sensitive scope | No | No | No | Yes |
| Hidden, everyone | Yes | No | No | Yes | Yes |
| Birthday year, non-sensitive | Yes | No | No | No | Yes |
| Archived | No | No | No | No | No |

## Validation

Text is one line of at most 200 characters; long text allows newlines and tabs up to 2,000 characters. Dates are real `YYYY-MM-DD` dates in 1900–2100, select values are option keys, and phone numbers normalize to international `+` plus 7–15 digits. URLs require HTTP(S), a host, no credentials, and at most 2,000 Unicode characters, matching PostgreSQL plaintext storage; handles validate according to Telegram, GitHub, GitLab, X, or Mastodon conventions. Empty strings clear values; multi-field updates are atomic. Update validation checks the whole batch before decrypting existing values; those reads, all writes, and the authorized response are audited and committed in the same transaction. A response audit failure rolls back every mutation and returns no entries. All values must be valid Unicode and cannot contain NUL or other disallowed control characters.

## API

`/api/v1/profile-fields` and `/api/v1/profile-field-library` require `people.manage`. The person's `/api/v1/people/{personId}/profile` supports authenticated GET and PATCH with service-enforced visibility, self/admin edits, and audit entries containing changed keys, never values. Sensitive values are never returned unless their read audit transaction commits.
