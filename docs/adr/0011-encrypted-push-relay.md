# 0011. End-to-end encrypted push via an open-source relay

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Mobile push must go through APNs/FCM using the app publisher's credentials, so self-hosted servers need a relay. Users must be able to trust that the relay can't read their company's notifications.

## Decision

- The self-hosted server **encrypts each notification for the target device's key**. The relay (`push.orglane.app`) forwards opaque envelopes only, and the device decrypts before display (Notification Service Extension on iOS, data messages on Android).
- **The relay is open source**, so anyone can verify what it does.
- The free tier for Community has generous limits; Link removes them.
- Without the relay, the Inbox, Web Push and Telegram still work.
- Companies that need full independence can build the app with their own push credentials (enterprise path).

## Consequences

- Strong privacy story for open-source users and EU customers.
- Slightly more native mobile code (notification extension, key management).
- The value of Link comes from the app's credentials and limits, not from secret code.
