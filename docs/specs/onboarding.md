# Onboarding checklist (#29)

The public API serves the employee checklist at `GET /api/v1/me/onboarding`, finishes welcome through `POST /api/v1/me/onboarding/finish`, and exposes caller-visible progress for one person at `GET /api/v1/people/{personId}/onboarding` with `people.manage`. Every client uses this contract. No provider or module is implemented, and the mobile step is absent until M4.

`onboarding.Step.Check` is the extension boundary for future compiled provider/module adapters. It receives the current profile viewer and subject, derives completion, and returns a stable ID, title, description, required flag and authorized local action URL. Returning nil omits a step. The ordered registry rejects duplicate IDs, unsafe links and a required self-service step without an action. Future provider IDs must be namespaced. Each source must apply its own operation permissions and omit unauthorized metadata before returning results.

Built-in steps derive completion from existing profile values, stored avatar presence and registered passkeys on every request. Optional steps remain incomplete when skipped and reopen when the actual data is removed. The profile action links to the directory's existing self/admin profile editor and raster upload control; passkeys use the existing security screen and registration machinery.

Required profile completion uses `profile.Service.Profile` and its centralized visibility, edit policy, encryption and transactional sensitive-read auditing. Only required fields the subject can see and edit block their welcome. Visible required fields that need an administrator form an optional follow-up step, with an action only for an authorized administrator. Unreadable sensitive fields do not create a blocker or expose a count, key, label, presence bit or value. The API never returns profile values or field metadata in progress. Administrator results are relative to their own access; the UI explains that private fields are excluded. Other people's welcome-dismissal state is never exposed, because it could otherwise reveal completion of a field the administrator cannot read.

Migration 00011 stores only `person_onboarding.welcome_finished_at` and backfills finished markers for invitations already accepted before the upgrade, so existing employees are not forced through a newly introduced welcome. Unaccepted invitations still start welcome when accepted after the upgrade; the marker does not complete optional steps. Pending welcome is derived from an accepted invitation without a finished record. Setup and ordinary sign-in retain their normal routes, OIDC continuation is unchanged, and the existing optional passkey offer retains Skip. An interrupted invited welcome resumes after another sign-in. Finishing checks current actionable requirements, commits the marker and audit atomically, and is idempotent. Later requirement or profile changes update the checklist without restarting finished welcome. Optional setup can continue from Home. Welcome keeps a safe original workspace destination when sign-in had one, including after session expiry inside a welcome continuation. Nested local welcome links are unwrapped with validation at every level and a bounded depth, retaining the final workspace destination without welcome loops or external redirects.

After a successful user save completes the required checklist, welcome transfers keyboard focus to the enabled Continue button after the current progress refresh. Initial and background reads do not request focus. Each checklist status remains a stable polite, atomic live region so completion changes can be announced.

The UI uses the existing components, tokens and English i18n catalog. Welcome embeds the required-field editor, Home shows the checklist, and authorized administrators see progress on the existing person page. Design polish is deferred; screenshots and the real invited-person Playwright scenario support independent UX review.

## Verification coverage

The real-Postgres HTTP scenario exercises invitation acceptance, interrupted/repeated sign-in, required edits, administrator-only follow-up, optional skip, idempotent finish, sensitive-progress invariance for unauthorized admins, mandatory sensitive-read audit failures, welcome-write rollback, profile clearing, avatar addition/removal and a real WebAuthn registration/removal. Existing migration up/down tests include the new table. Registry unit tests cover live completion, omission, duplicate IDs and unsafe/impossible actions. UI tests cover required blocking, optional continuation, CSRF and the session guard. Playwright uses the real server and Postgres to complete an invited profile, skip optional steps, return after sign-in and follow the real directory avatar upload action.

## UX review evidence

The [grouped PR attachments](https://github.com/orglane/orglane/pull/39#issuecomment-6616403498) preserve the verified captures from source `6f132e09407462b7d20d78b20dc1afc2ac764238` and their SHA-256 hashes. Review images are outside Git; future captures are written to the ignored PR assets directory.

- [Welcome with required profile information missing](https://github.com/user-attachments/assets/458d3b89-9776-4e16-87bc-bd854f6bd4fb)
- [Welcome at 360px](https://github.com/user-attachments/assets/387f8cd4-e1a3-4856-bc09-123b2fc7d3eb)
- [Welcome after required completion](https://github.com/user-attachments/assets/c93bf78b-dbb1-4fb6-b6b9-75235bb18411)
- [Home with optional steps skipped](https://github.com/user-attachments/assets/1d8f13ef-d1f9-4b65-913e-643b1650b304)
- [Permissioned progress on the existing person page](https://github.com/user-attachments/assets/214363d9-6667-4d4d-8052-ef9a6a941f13)
