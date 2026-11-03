# Onboarding checklist (#29)

The public API serves the employee checklist at `GET /api/v1/me/onboarding`, finishes welcome through `POST /api/v1/me/onboarding/finish`, and exposes caller-visible progress for one person at `GET /api/v1/people/{personId}/onboarding` with `people.manage`. Every client uses this contract. No provider or module is implemented, and the mobile step is absent until M4.

`onboarding.Step.Check` is the extension boundary for future compiled provider/module adapters. It receives the current profile viewer and subject, derives completion, and returns a stable ID, title, description, required flag and authorized local action URL. Returning nil omits a step. The ordered registry rejects duplicate IDs, unsafe links and a required self-service step without an action. Future provider IDs must be namespaced. Each source must apply its own operation permissions and omit unauthorized metadata before returning results.

Built-in steps derive completion from existing profile values, stored avatar presence and registered passkeys on every request. Optional steps remain incomplete when skipped and reopen when the actual data is removed. The profile action links to the directory's existing self/admin profile editor and raster upload control; passkeys use the existing security screen and registration machinery.

Required profile completion uses `profile.Service.Profile` and its centralized visibility, edit policy, encryption and transactional sensitive-read auditing. Only required fields the subject can see and edit block their welcome. Visible required fields that need an administrator form an optional follow-up step, with an action only for an authorized administrator. Unreadable sensitive fields do not create a blocker or expose a count, key, label, presence bit or value. The API never returns profile values or field metadata in progress. Administrator results are relative to their own access; the UI explains that private fields are excluded. Other people's welcome-dismissal state is never exposed, because it could otherwise reveal completion of a field the administrator cannot read.

Migration 00011 stores only `person_onboarding.welcome_finished_at`. Pending welcome is derived from an accepted invitation without a finished record. Setup and ordinary sign-in retain their normal routes, OIDC continuation is unchanged, and the existing optional passkey offer retains Skip. An interrupted invited welcome resumes after another sign-in. Finishing checks current actionable requirements, commits the marker and audit atomically, and is idempotent. Later requirement or profile changes update the checklist without restarting finished welcome. Optional setup can continue from Home. Welcome keeps a safe original workspace destination when sign-in had one.

After a successful user save completes the required checklist, welcome transfers keyboard focus to the enabled Continue button after the current progress refresh. Initial and background reads do not request focus. Each checklist status remains a stable polite, atomic live region so completion changes can be announced.

The UI uses the existing components, tokens and English i18n catalog. Welcome embeds the required-field editor, Home shows the checklist, and authorized administrators see progress on the existing person page. Design polish is deferred; screenshots and the real invited-person Playwright scenario support independent UX review.

## Verification coverage

The real-Postgres HTTP scenario exercises invitation acceptance, interrupted/repeated sign-in, required edits, administrator-only follow-up, optional skip, idempotent finish, sensitive-progress invariance for unauthorized admins, mandatory sensitive-read audit failures, welcome-write rollback, profile clearing, avatar addition/removal and a real WebAuthn registration/removal. Existing migration up/down tests include the new table. Registry unit tests cover live completion, omission, duplicate IDs and unsafe/impossible actions. UI tests cover required blocking, optional continuation, CSRF and the session guard. Playwright uses the real server and Postgres to complete an invited profile, skip optional steps, return after sign-in and follow the real directory avatar upload action.

## UX review images

- [Welcome with required profile information missing](../screenshots/onboarding-welcome-required.png)
- [Welcome at 360px](../screenshots/onboarding-welcome-narrow.png)
- [Welcome after required completion](../screenshots/onboarding-welcome-complete.png)
- [Home with optional steps skipped](../screenshots/onboarding-home-skipped.png)
- [Permissioned progress on the existing person page](../screenshots/onboarding-admin-progress.png)
