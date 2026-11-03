# People directory (#25)

The directory uses the public API and existing identity/Teams/profile services. It searches only public base fields (name, username, work email, title), never custom values or sensitive metadata. PostgreSQL's pg_trgm index supports case-insensitive literal substring search without extra infrastructure. Results use UUID keyset pagination (stable creation order), capped at 100; team and role filters combine with search.

`updated_since` is inclusive, returns current rows including offboarded tombstones (ID/status/timestamp, all other fields empty), and cannot combine with search/team/role filters: membership and search removals require a full filtered refresh. Each page has a content-based private ETag, `updatedAt`, and a next cursor. Pagination is over current state, not a frozen snapshot; consumers merge by ID, overlap incremental refresh windows, and periodically revalidate all pages. No custom values enter the list or its cache validator. A separate public directory timestamp advances only for public base/team/role/avatar changes, so private contact-email updates do not enter public validators or deltas.

The person detail reuses the profile service for all custom field reads/edits. Base fields are editable by self (display name/title) or `people.manage` (also username/work email/manager); authorization is checked before writing. Manager changes serialize on a transaction advisory lock before row locks, validate the entire ancestor chain, and audit changed field names with the person transaction.

Uploads accept at most 5 MiB of PNG/JPEG/WebP, check media type and decoded dimensions before allocation, cap input at 16 megapixels, resize proportionally to 512px, and encode a fresh PNG capped by the existing 256 KiB storage constraint (fall back to a smaller raster if needed). SVG remains available only through the existing trusted/demo path. The existing avatars table and audit transaction are retained. `golang.org/x/image` (BSD-3-Clause) supplies WebP decoding and scaling.

The UI uses existing tokens/components/i18n: search, filters, next/previous pages, linked profiles, public header/contact details, manager link, base edit form, avatar upload and the existing custom field editor. No design system or visual redesign is introduced.

## Implementation and verification

1. Define OpenAPI, migration 00009 and SQL queries before implementation; regenerate checked-in Go/TS outputs once the exclusive slot is granted.
2. Write real Postgres tests for 1,200-person substring searches, literal wildcards, combined team/role filters, bounded non-overlapping pages, incremental membership/avatar/status changes, manager cycles including concurrent races, and audited updates. Add HTTP permission/cache/upload tests and raster security tests.
3. Implement identity operations, contract handlers and minimal UI; preserve profile field visibility and Teams behavior.
4. Verify fmt, generated-code consistency, lint, tests, build and security; run real demo Playwright and capture directory/profile at 1440/768/360 in both themes. Submit a Conventional Commit and PR for independent code/UX and CodeRabbit review, without approving or merging.

Review focus: concurrent reporting cycles; SVG or mislabeled/truncated decompression bombs; cache tombstones/membership removals; member attempts to edit another person or protected base fields; user input containing SQL LIKE wildcards and offset-free traversal beyond 1,000 people.

## Trusted transaction reuse

`(*identity.Service).UpdatePersonTx(ctx context.Context, q *store.Queries, actor audit.Actor, id uuid.UUID, in identity.PersonUpdate) (store.Person, error)` uses the same normalization, manager validation, no-op semantics and audit as `UpdatePerson`, within the caller's transaction. `q` must come from `store.New(tx)` for that transaction; this helper never commits independently. The caller must return errors and roll back the outer transaction so base-person fields, public-cache timestamps and audit entries roll back together.

`identity.AcquireReportingLock(ctx context.Context, q *store.Queries) error` must be called before any person row locks when the outer transaction may change reporting lines. Callers with a grant lock take it first, then reporting, then team/person/role/invitation row locks in their established order. `UpdatePersonTx` reacquires the transaction reporting lock for manager changes, which is safe when the caller already holds it; callers must never first acquire it after locking a person. `ContactEmail` is invitation/import-owned and remains outside `PersonUpdate`. These are trusted domain helpers, with permission checks retained by their public API callers.
