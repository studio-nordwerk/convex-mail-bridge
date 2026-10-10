# Known limits

What the bridge and the package do not do, where they are stricter than you might expect, and
what has not been verified. For the settings mentioned here, see
[`../bridge/README.md`](../bridge/README.md); for the interface to the Convex app,
[`edge-contract.md`](edge-contract.md); for running the bridge, [`operations.md`](operations.md).

## Scope of this release

The bridge serves two folders, `INBOX` and `Sent`. In scope:

- reading messages,
- flags (`\Seen`, `\Flagged`, `$Forwarded`, ...),
- delete, which hides the message from mail clients and leaves the app's own data alone,
- IDLE, so new messages appear without fetching,
- sending through SMTP submission.

Not in scope:

- **No search in message content** (details below).
- **No `MOVE` and no `COPY`.**
- **No folder management:** mail clients cannot create, delete or rename folders.
- **Drafts and trash stay local in the mail client.** Set them to be kept on the device (in
  Apple Mail: Mailbox Behaviors). The bridge does not store drafts, and "delete" does not
  move a message to a server-side trash.
- **Sending is the app's job.** The package passes the envelope and the raw message to the
  app's `submit` callback; sending, protection against sending the same message twice, and rate
  limiting are done there. The package does not send anything itself.
- **Revision 1.2 of the contract (more folders) is in progress.** The Go bridge here implements
  version 1. See "Revision 1.2: folders (in progress)" in [`edge-contract.md`](edge-contract.md).

## Details

The bridge builds on `go-imap` v2 (a pre-release, `v2.0.0-beta.8` at the time of writing) and
`go-smtp`. Several limits below are limits of those libraries.

- **No content search.** The bridge answers `SEARCH` only where the mirror has the exact answer:
  UIDs, flags, arrival date and size, combined with `NOT` and `OR`. A search in the subject, the
  sender, the headers or the text (`SUBJECT`, `FROM`, `TO`, `HEADER`, `TEXT`, `BODY`,
  `SENTSINCE`, ...) is answered with `NO [CANNOT]`. Apple Mail on the Mac searches locally;
  "continue search on the server" on the iPhone finds nothing. One quirk of the library:
  `SMALLER 0` arrives as "no size limit".
- **No `MOVE`, no `COPY`**, and no creating, deleting or renaming folders: `NO [CANNOT]`.
  `MOVE` is not announced. `SUBSCRIBE` is allowed for the two existing folders (it changes
  nothing) and for nothing else.
- **`APPEND`** only into `Sent`, and only for a message that was sent through the app. The
  bridge sends the Message-ID as it stands in the header (the edge normalizes it) and the
  SHA-256 of the bytes (the edge does not compare it with anything). Everything else gets
  `NO [CANNOT]` and is counted in `appendsRejected`.
- **No `CONDSTORE` or `QRESYNC`** (the library cannot do them on the server side). Mail clients
  fetch the flags of the folder at every `SELECT`; they are served from the mirror.
- **`ID` is neither announced nor answered.** `go-imap` v2 does not know the command on the
  server side and has no place where it could be added. After login it answers `BAD`; before
  login it drops the connection on unknown commands. Apple Mail sends `ID` only when the server
  announces it (Apple TN3191).
- **The keepalive in IDLE** is a repeated `* FLAGS (...)` line. That is what the library allows;
  a `* OK Still here` is not possible. Without a selected folder there is no line in IDLE.
- **`.SILENT`:** also after `STORE ... FLAGS.SILENT` the connection receives the new flags as an
  ordinary notification. This is allowed and keeps the order of notifications per connection
  correct.
- **`EXPUNGE` under `EXAMINE`** answers `OK` and does nothing (the library calls the same code
  for `CLOSE`).
- **Concurrent commands** of one connection run one after another (library).
- **Whoever does not listen is dropped.** A connection that has selected a folder and does not
  collect its notifications (no IDLE, no commands) is closed with `BYE` as soon as more than
  2,000 notifications have waited across two polls, or 20,000 at once, or when all connections
  together would hold more than 300,000 waiting notifications. The mail client reconnects; the
  mirror stays.
- **At most 32 `BODY[...]` items per `FETCH`**, and parts at most 24 levels deep: otherwise
  `NO [LIMIT]` or `BAD`. `BODYSTRUCTURE` describes at most 24 levels and 2,000 parts; what lies
  deeper appears as an attachment.
- **Stricter than the library:** a line that ends in the middle of a quoted string, and a
  non-synchronizing literal (`{n+}`) that the library rejects, end the connection. Otherwise
  the bytes that follow would be read as commands.
- **SMTP:** only `AUTH PLAIN`, no SMTPUTF8. `VRFY` answers the same for every address (`252`)
  and there is no `EXPN`. Lines over 2,000 bytes are rejected. An SMTP connection ends with its
  session (expiry, revocation), and a transfer ends after at most `SMTP_READ_TIMEOUT_SECONDS`.
- **Two instances** each have their own mirror. It stays correct because each one asks the edge.

## Hardening still open before serving untrusted users

Not needed for a single trusted person, but open before you serve people you do not control:

1. **A throttle in the bridge itself.** Today every login with a new password goes to the edge,
   which counts and throttles. Before untrusted users: a delay and a counter per client address
   in the bridge too.
2. **`APPEND` and SMTP `DATA` reserve their working memory before reading.** A logged-in client
   can hold that memory for minutes. Reserve only after the first bytes, or lower the time limit.
3. **A machine with 1 GB of memory** as soon as more than a handful of mailboxes read large
   messages at the same time.
4. **Protect `/metrics` with a token** if the network the admin port is on also holds apps of
   other people (on Fly.io: other apps in the same organization can read it).

Also required before the first foreign mailbox: set `PROXY_TRUSTED_CIDRS`
(see [`operations.md`](operations.md)).

## Not verified

- **Apple Mail itself, Mac and iPhone, end to end.** The local setup in the bridge README has
  not been walked through with Apple Mail. It was cross-checked with `imaplib`, `smtplib` and
  mbsync. This repository records no run with Thunderbird or Outlook either.
- **TLS and the PROXY protocol behind a real hosting proxy.** The automated tests do not cover
  them. The Compose example was smoke-tested by hand on one machine: HAProxy with a self-signed
  certificate in front of the development bridge, TLS on both ports, PROXY protocol v2, a
  sign-in, and the rejection of a connection without the header. A hosting provider's proxy, a
  real certificate and long-lived connections are not covered. Check them yourself with the
  steps in [`operations.md`](operations.md): reachability, the restricted PROXY header, and the
  29-minute IDLE test.
- **Load:** many connections and large mailboxes over the network.
- **Sending through a provider.** The SMTP side of the bridge is tested against the stub edge and,
  in the run described below, against the example app, whose `submit` records the message without
  handing it to a sending provider. Delivery through a real provider is your app's part and is
  not covered here.

What has been verified against a real edge: `bridge/test/against-edge.sh` passed 77 checks, with
none failed and one skipped (`Sent` was empty), against a self-hosted Convex backend running the
original production edge, with two mailboxes of 60 and 6 messages. It covered lists and counters,
every message byte for byte, flags set and unset, IDLE (a new message was announced 0.4 s after
delivery), `UID EXPUNGE`, reconnecting without loading again, the separation of the two mailboxes,
and `mbsync` with a second run that made no fetches. The edge rejected no request as invalid
(`400`, `413`, `500`: 0).

The open-source client layer in `src/client` is a re-cut of that edge. Besides its own test suite,
which drives the example app through every route, the probe has been repeated against it: the
example app on a local, self-hosted Convex backend with the Go bridge in Docker in front of it.
`bridge/test/against-edge.sh` passed 77 checks, with none failed and one skipped (`Sent` was
empty), with two mailboxes of 12 and 4 messages, including IDLE, `UID EXPUNGE`, the separation of
the two mailboxes and `mbsync`; the edge rejected no request as invalid. A separate script sent a
message over SMTP through the example app's `submit` and read the copy back from `Sent` byte for
byte. Not covered by that run: a real mail client, a cloud deployment, TLS and the PROXY protocol,
the production image of the bridge, shared mailboxes, the backfill, the cleanup cron and the
throttle (those are covered by the test suite only).
