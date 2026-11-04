# People import

ADR 0015, issues #27, #28 and #38. File adapters implement `peopleimport.Source` and
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
apply returns the saved result.

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
