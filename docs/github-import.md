# GitHub organization people import

This M1 feature imports source data through the same encrypted staging, mapping,
preview, apply, audit and invitation pipeline as CSV/XLSX. It is not a GitHub
provider integration and does not authenticate, verify ownership, provision an
account or grant GitHub access. Imported account associations always remain
`pending`; M2 must establish ownership independently.

## Token and API access

Use a short-lived fine-grained personal access token whose resource owner is the
organization, with organization **Members: read** permission. No repository write,
email write or classic `repo` scope is needed. The token owner should be an
organization member so concealed members are included; organization approval or
SSO policy may also apply. Orglane can only import the membership visible to that
token and does not certify that GitHub returned every concealed member.

Official GitHub documentation checked 2026-11-04:

- [List organization members](https://docs.github.com/en/rest/orgs/members#list-organization-members)
- [Get a user using their ID](https://docs.github.com/en/enterprise-cloud@latest/rest/users/users#get-a-user-using-their-id)
- [Create a fine-grained PAT](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token)
- [Rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)

The browser sends the token once in the JSON POST body, not in a URL. Transient
form state clears when fetching starts, finishes or the source selection changes.
Credentials never enter React Query variables/data, browser persistent storage,
background jobs, staging, pending links, audit metadata or error text. Memory is
released normally by the runtimes; this is not a claim of secure memory erasure.

Production requests go only to `https://api.github.com`, with redirects disabled.
Page URLs are constructed locally, ignoring all Link headers and source URLs.
Fetching continues until an empty page, including after a short nonempty page.
Fetches are sequential: one organization lookup, pages of 100 members, and one
public-profile lookup per stable member ID. There are at most 2,022 requests,
16 KiB response headers, 1 MiB per response body, 32 MiB aggregate response body bytes (one extra byte may be read to detect overflow), 10 seconds per request and
five minutes total (including staging). The authorized fetch route alone extends
the HTTP response deadline to 330 seconds; reverse proxies must allow this duration. There are zero automatic retries. HTTP
401/403/429, exhausted remaining budget, cancellation, malformed responses, repeated
IDs, timeout or any failing page/profile abort the fetch before staging. Wait for
GitHub's rate-limit reset before manually retrying. An org changing during a fetch
can yield an inconsistent snapshot; duplicates fail, but administrators should
review and rerun after membership changes settle.

GitHub runs support 2,000 members. Exactly 2,000 requires an extra empty page;
2,001 fails without a staged partial run. CSV/XLSX remain limited to 1,000 rows.
Only the internal GitHub source obtains the larger staging allowance; persisted
`github:<numeric organization ID>` runs obtain the corresponding mapping/skip and
resumed processing limits. Request, column, cell and file limits stay in force.

## Mapping and identity

Public email defaults to contact email. It is optional and conveys no verified
work-address or account-ownership claim. New people still require a username,
full name and work email; missing values produce existing `invalid_fields`
conflicts. An administrator may explicitly map the public-email column to work
email after assessing its suitability, or create/correct the person through the
normal directory/CSV flow and reimport. Existing people match by supplied email
and stable external ID, never display name or login alone. All supplied identities
must agree. The external-ID column is mandatory and cannot be remapped away for
GitHub runs. Absent names/emails never receive fabricated replacements.

The numeric organization ID namespaces stable numeric member IDs, surviving org
and user login changes. Pending links preserve provider, organization ID, member
ID, current login and avatar URL. Conflicting person assignments fail within row
savepoints. Avatar URLs are source evidence only: no remote image is fetched or
rendered; this does not replace the person's normalized avatar. Existing invitation
ceilings, current grants and derived onboarding rules apply without alteration.

## Manual real-API smoke

1. Use a disposable/test organization with known public and concealed members,
   including a member with no public email. Create the short-lived read-only PAT
   described above. Do not put it in shell history, screenshots or reports.
2. Sign in as an authorized people administrator, open People → Import, choose
   GitHub organization, and fetch with the organization slug and token.
3. Verify the password field clears, row count matches membership visible to the
   token, and mapping defaults public email to contact email. Review missing-data
   conflicts. Explicitly map a suitable test address to work email for a new test
   person, or match an existing disposable person.
4. Preview and apply. Confirm invited status and the normal onboarding/invitation
   flow, and confirm database pending links remain pending. Reimport and verify
   stable matching; rename a test GitHub login and repeat to verify the same
   numeric external identity remains associated with the same person.
5. Revoke the token; retry and verify a fixed fetch failure creates no run.
   Inspect local audit and staging records for absence of the token, without
   publishing credential values. Revoke any remaining token and remove test data.

Automated acceptance uses synthetic httptest source data plus real PostgreSQL.
The UI acceptance binary alone is built with `githubimporttest`, which injects a
local httptest fixture. Production builds contain no configurable source URL or
fixture server. Automated tests never need or request a real GitHub token. A passing
fake-source test is not evidence of live GitHub or provider integration support.
