# Changelog

## 0.1.0 (unreleased)

First public version.

- Component `mailBridge`: UID ledger per mailbox and folder (UIDs, flags, change states,
  tombstones, counters), device passwords, session tokens, throttle for failed sign-ins, set-up
  links.
- Client layer `MailBridge`: write path (`enable`, `place`, `setSeen`, `remove`), device
  passwords, the edge functions and routes of contract v1 (`/bridge/v1/*`), housekeeping,
  configuration profiles for Apple Mail.
  - Sending: `submit` receives a handle to its session (`tokenHash`), and `edge()` returns a
    `session` function to check it again in the mutation that records the message; `submit` may
    answer `{ status: 'session' }`. Without `submit`, `/submit` answers `unavailable` and
    `allowSubmit` is not asked.
  - `/raw` with the default loader answers `404 gone` when `rawLocation` returns something that
    is not an id of Convex file storage.
  - The type declarations avoid `Uint8Array<ArrayBuffer>`, so they do not require TypeScript 5.7.
- Bridge (Go): IMAP and SMTP submission for `INBOX` and `Sent`, IDLE, in-memory mirror and raw
  cache, PROXY protocol, metrics.
- Example app and deployment examples for Fly.io and Docker Compose with HAProxy.

Not in this version: folders beyond `INBOX` and `Sent` in mail clients, `MOVE`, content search,
client wrappers for the QR set-up flow.
