# System email and invitations

System email uses SMTP, independently of employee mailboxes (ADR 0014). No
additional Go or TypeScript dependency is needed: `net/smtp`, `crypto/tls`,
`html/template`, `text/template` and the existing River queue handle delivery.
Mailpit is a development and test service only.

An administrator opens **People → Invite people** to invite someone, or
**Account menu → System email** to configure SMTP and send a test message.
Email settings require `instance.manage`; invitations require `people.manage`.
The inviter must hold every permission in the role they grant; the same check
applies to resend and revoke, preventing an Admin from obtaining a pending
Owner's invitation link. SMTP settings
never return the password. A blank password field keeps the existing password
when the username is unchanged; clearing the username disables authentication.

The contact address is for invitations and future recovery. It is distinct
from the work address, which is an identity and does not require a mailbox.
Work email defaults to `domain.work_email_template: "{username}@{base}"`.
Admins can override that address when inviting someone.

## Delivery and configuration

Settings saved through the API encrypt the SMTP password with an AEAD key
derived from `identity.master_secret`, using a purpose distinct from other
encrypted data. Keep that secret stable and include it in instance backups.
Installations can instead configure email in YAML or environment variables:

```yaml
email:
  smtp:
    host: smtp.example.com
    port: 587
    tls: starttls
    username: mailer
    # Supply the password using ORGLANE_EMAIL_SMTP_PASSWORD.
  from: "Company <no-reply@example.com>"
  max_attempts: 6
  retry_delay: 30s
identity:
  invitation_lifetime: 168h
domain:
  work_email_template: "{username}@{base}"
```

Every setting supports the usual `ORGLANE_` environment override, for example
`ORGLANE_EMAIL_SMTP_HOST` and `ORGLANE_EMAIL_FROM`. A configured SMTP host takes
precedence over database settings and makes them read-only in the UI. Remove
that override to manage them in the UI again. Without either configuration,
the admin receives a link to copy and share; the link is shown only when
issued. Resend creates another link if the first was lost.

TLS modes are `starttls` (587 by default, mandatory upgrade), `tls` (465,
implicit TLS) and `none` (25, only for a trusted relay). Certificates and server
names are verified; credentials are refused over unencrypted connections
except to localhost. Development uses Mailpit on SMTP port 1025 and its inbox
at `http://localhost:8025` (`make dev`). The production Compose bundle does not
include a mail server.

The outbox row and River job commit in the same transaction as the invitation.
The outbox stores encrypted message content, including the invitation link;
River arguments contain only its outbox ID. Successful delivery, final failure
and cancellation erase that content. Delivery failures record bounded error
details, attempts and state for the UI. Retries double from 30 seconds up to
one hour, for six attempts by default. Test messages also use the queue and
outbox; the settings request waits for the first delivery result. SMTP
acceptance means the receiving server accepted the message, not that it reached
the recipient's inbox. SMTP delivery is at-least-once: a process failure between
SMTP acceptance and recording success can send a duplicate message.

Templates are multipart HTML and plain text with no remote assets or tracking.
The recipient's language is captured from the invitation request's
`Accept-Language`. The catalog lives in `core/internal/mail/locales`; English
is the current complete language and fallback, consistent with the roadmap.

## Invitation lifecycle

Links contain a cryptographically random token in `/invite#oli_…`. The fragment
keeps it out of HTTP access logs and Referer headers. Preview and acceptance
send it in a POST body. The invitation table stores only a SHA-256 hash.
Links last seven days by default (configurable from one hour to 90 days).

An invitation creates an `invited` person with their role and selected teams.
Initial team memberships use the `member` role and commit with the person,
role assignment and invitation. Team locks are taken in UUID order before
creating the person, matching the teams service's lock order; archived or
missing teams reject the complete operation. Selection supports up to 100
distinct active teams. Resending preserves the person's current memberships.
Acceptance checks
the token under locks, sets a policy-compliant password, activates the person
and creates a device-bound session. The existing passkey offer follows with
an explicit **Skip for now** action. Invitations cannot be accepted twice.

Resending replaces the token and expiry and cancels a queued earlier message.
Revoking invalidates the link and cancels queued delivery, keeping the person
in `invited` status. Resending a revoked or expired invitation makes it usable
again. Accepted invitations cannot be resent or revoked. Creation, resend,
revoke and acceptance are audited in the transaction; hourly identity
maintenance audits each unused expiry once. A message already being delivered
may still arrive after revoke, but its token cannot be used. Resend and revoke
also record a prior unrecorded expiry in their transaction, so changing an
expired invitation before maintenance cannot lose that event.

## SMTP authority and invitation grants

SMTP settings remain editable with `instance.manage`, as required for both
Owner and Admin workflows. An activation token grants the invited identity's
permissions, so an SMTP editor must not be able to intercept a token granting
permissions they lack. This includes custom bundles: holding `instance.manage`
does not imply the rest of the built-in Admin permissions.

Editable, database-managed SMTP therefore carries only grants contained in the
minimum permission envelope guaranteed to every SMTP editor: `instance.manage`
itself (or no administrative permission). Ordinary Member invitations continue
to use email. Grants with any additional permission use an explicit copy-link,
including Admin, Owner and custom privileged bundles. Server-configured SMTP
is immutable through the API and can carry all grants. The invitation options
API describes delivery separately for each grantable role; the UI explains
when a configured email service requires a direct link for the selected grant.

Each token persists distinguishable link/settings-email/config-email channel
provenance and its issued permission ceiling. Acceptance rechecks that current
permissions fit that ceiling; promoting an invited person or enlarging a role
requires resend. A privileged copy-link stays usable even while editable SMTP
is active. Legacy tokens without a trusted envelope are limited to the SMTP-editor
envelope, including old direct links; additional current permissions require
resend because the original ceiling is unknown. New explicit privileged
copy-links remain usable independently of current SMTP settings.

Every queued delivery attempt resolves one effective SMTP settings snapshot,
checks the current grant against both its issued envelope and that snapshot's
authority, and sends using the same snapshot. A settings-source transition,
legacy queued privileged email or later role promotion cannot redirect an
additional permission grant to a lower-trust relay. Unsafe attempts fail
terminally, clear encrypted content and tell the inviter to resend via a safe
link. Resending cancels the old message and chooses a new channel/envelope.

Relay diagnostics are untrusted and may echo message bodies, credentials or
activation URLs. They are discarded in favor of locally selected, bounded
failure categories; unknown preparation errors are also reduced to a safe
category. Configured credentials/authentication encodings and token patterns
are redacted before persisting, logging, returning API diagnostics or returning
worker errors to River. Raw SMTP rejection text is never retained as a wrapped
cause. This trades some relay troubleshooting detail for protecting bearer
capabilities and credentials.
