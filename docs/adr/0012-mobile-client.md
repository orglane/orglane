# 0012. Mobile client: discovery, QR sign-in, devices, link handoff

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

The mobile app is the center of the product vision: one place for everything in the company.

## Decision

- **One app in the stores** ("Orglane"). After sign-in it shows the company's name, logo and colors. A fully white-labeled app is a later, paid option.
- **Sign-in:**
  1. scan a QR code from the web (server URL + single-use pairing code);
  2. enter a work email → discovery via `https://<domain>/.well-known/orglane` or the cloud directory → passkey sign-in in the system browser;
  3. enter the server URL manually.
- **Devices:** every session is bound to a device. Devices are visible to the user and to admins, and can be revoked with a remote wipe of app data (part of offboarding).
- **The data model supports multiple companies**; the switching UI comes later.
- **v0.1 screens:** Home (widgets), Inbox, People (cached offline), Apps, Profile & settings, plus the onboarding checklist.
- **Native vs web:** lists and rich cards are native. Deep object details (a PR, for example) open on the web for now.
- **Links:** open in the **external browser by default** through a single-use **handoff link** that establishes the SSO session; a setting switches to the in-app browser.
- **Returning to the app:** Universal Links / App Links on `orglane.app` (company domains can't be listed in the app's build-time entitlements), plus the `orglane://` scheme.

## Consequences

- Zero-typing onboarding is possible (QR).
- Session handoff must be designed into the core from the start, with short-lived, single-use tokens.
