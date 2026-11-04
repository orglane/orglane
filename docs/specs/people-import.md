# People import

ADR 0015, issues #27, #28, #38 and #42. File adapters implement `peopleimport.Source` and
produce bounded untrusted records; adapters never write people. File external
identifiers use one stable namespace across CSV and XLSX. Other sources must use
their own stable namespace.

The public API stages an uploaded file, saves an explicit column mapping and
preview, then applies that preview. Every row and header is encrypted with a
separate derived key, bound to its run and row. APIs and error exports disclose
row numbers, decisions and fixed error codes only; column captions only disclose
recognized field names. Unknown headings are numbered. Mapping never reads
source values back into the browser. Keep the original file to correct failures.

Blank cells preserve existing data. Role mapping adds a permission bundle, with
caller-versus-target and caller-versus-requested-grant checks under the grant
lock; it does not silently remove existing roles. Team mapping adds memberships,
using semicolon-separated names, and optionally creates unknown teams. Duplicate
team names are ambiguous. Manager email must resolve to an existing person;
import managers first. Existing invitation contact destinations cannot be changed
by import; changing an invitation destination requires a separate lifecycle flow.

Matching checks work email, contact email and source-scoped external ID in that
priority order; ambiguity or identifiers pointing to different people is a
conflict. Duplicate identifiers within a file are conflicts, excluding rows the
operator explicitly skips. Reimports produce no duplicate people or memberships. Preview
records a version of the matched person's state and related grants, plus the
resolved manager and team identities, absence and ambiguity. Team resolution
is compared against an initial snapshot under the import locks, before any
apply writes; teams auto-created by earlier rows of that same apply may be
reused without treating simulated preview UUIDs as stable identities. Apply
revalidates identities, grants, field definitions and versions. A stale row fails
and must be previewed again in a new run. Applied runs are immutable; repeated
apply returns the saved result until the run expires (see Lifecycle).

CSV row numbers are the physical starting lines, including blank lines and
quoted multiline cells; XLSX uses the original worksheet row numbers.

File limits: 64 KiB for strict mapping JSON, 5 MiB upload, 32 MiB total actual ZIP expansion, 100 ZIP entries,
1000 data rows, 100 columns and 4096 bytes per cell. CSV supports UTF-8,
BOM-marked UTF-16 and Windows-1252, and detects comma, semicolon, tab or pipe.
XLSX reads the first worksheet, rejects formula rows without evaluating formulas,
and never follows external relationships. Error CSV contains no source values
and escapes spreadsheet formula prefixes defensively.

All changes for a row, custom fields, optional invitation email and its result
are committed together with audit records. A failed row rolls back to a savepoint
and other rows continue. No invitation link, token, source value or sensitive
field value is written to audit, preview responses or exports. Optional delivery
uses the existing invitation channel/permission ceiling; a grant requiring manual
link delivery fails the row rather than exposing a link through import results.

GitHub organization imports use the same pipeline with a source-specific 2000-row
bound, preserving the file limits above. The public-email column is initially a
contact address; it is never proof of ownership or an invented work address.
The immutable external-ID mapping and numeric organization namespace preserve
matching across login changes. Pending account evidence is included in person
snapshots for stale-preview checks, and is written by the identity domain in the
same row transaction. See [GitHub import](../github-import.md) for API/token
bounds, minimum permissions, missing-data semantics and manual real-API smoke.

For every GitHub row matched to an existing person, including stable-ID reimports,
only pending account evidence and the source identity mapping may change. All
local base/contact/profile fields, roles, teams and manager are preserved even
when mapped. Supplied identities still participate in conflict checks before
mutation values are discarded; current grants and local-state stale snapshots
remain enforced. New-person mapping and file-source updates are unchanged.

## Lifecycle

Issue #42 fixes how long import data is kept. The retention periods are
constants (`peopleimport.StagingRetention`, `peopleimport.AppliedRetention`),
not settings. Every run response carries `expiresAt`.

| Data | Kept | Then |
|---|---|---|
| Unfinished run (staged or previewed): sealed headers, row payloads, previews, mapping, decisions | 24 hours after upload | Deleted entirely |
| Source material of an applied run: sealed headers, row payloads and previews, including unmapped, skipped and conflicted values | Until apply | Erased in the apply transaction |
| Value-free report of an applied run: captions, mapping, row numbers, decisions, error codes, results | 90 days after apply | Deleted entirely |
| Audit entries (run id, counts, row decisions and error codes, never values) | Indefinitely (append-only) | — |
| `people_import_identities` (source-scoped external-ID hashes → person) | Indefinitely | Follows the person (M5) |

Apply erases the source material in the same transaction that marks the run
applied and sets `applied_at`. Captions (recognized field names, empty for
unknown headings) are stored in plain text before the sealed headers go, so the
result view, a repeated apply (which returns the saved result) and the error
CSV work from value-free data only. The operator keeps the original file;
anything else requires a new upload.

Expiry is exact: once `expiresAt` has passed, read, preview, apply and the error
export return 404 even before cleanup removes the run. Expiry is checked with
the current clock after the import locks (and the run's row lock) are granted,
so a request that waited while the run expired also returns 404 and writes
nothing. The hourly import cleanup
job (`orglane.people_import_cleanup`, River, also on start) deletes expired runs
with their rows and records `import.expired` with the system actor and the state
of the run. It also erases the source material of runs applied by 0.0.1, which
did not erase on apply (`import.source_erased`); migration 00013 dates their
apply from the `import.applied` audit entry. Cleanup is idempotent.

`DELETE /api/v1/people-imports/{runId}` deletes a run at any time: any current
holder of `people.manage` may delete any run, the uploader included (reads stay
restricted to the uploader). It records `import.deleted` with the deleting
person, run id and state, never values, in the same transaction. Deleting a run
that is gone succeeds without an entry. The people, memberships, invitations and
profile changes an applied run made are not undone, and the identity matches
stay so a later reimport does not duplicate people.

Delete and cleanup take the grant lock that upload, read, preview and apply also
hold for their whole transaction, so a run is never deleted halfway through
being applied: a delete or cleanup waits for a running apply, and an apply that
waited for a delete or expiry returns 404 without changing anything.

The lifecycle of `people_import_identities` follows the person: it is handled
with offboarding and deletion in M5. Runs a person uploaded need no extra
handling there, because every run expires within 90 days of apply.
