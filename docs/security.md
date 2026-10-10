# Security model

## What is protected by what

| Asset | Protection |
| --- | --- |
| A user's mail | a device password per mail client, checked by the edge; access callbacks of your app on every request |
| The edge (`/bridge/v1/*`) | a secret shared by bridge and Convex deployment |
| Traffic between mail client and bridge | TLS, terminated at the proxy in front of the bridge |
| Traffic between bridge and Convex | HTTPS |

## Device passwords

- Created by your app with `newDevicePassword`: a prefix and 192 bit from a cryptographic random
  source, written as 39 base32 characters. The user sees it once.
- Stored only as SHA-256, in the component. A plain hash is appropriate here because the
  password is long and random; **do not let users choose device passwords**, a human-chosen
  password would need a slow, salted hash.
- One per device. At most 10 active ones per person and mailbox. Each has a label and a "last
  used" time (updated at most hourly).
- Scope: one mailbox. A device password does not sign in to your app and opens no other mailbox.
- Revocation is immediate: the sessions of the password are deleted in the same mutation, every
  later request with one of them gets `401 session`, and the bridge closes the connection at the
  next poll (every two seconds while connections are open). Revoked rows are kept for 30 days and
  can never be reused.
- If a mailbox is handed to another owner, the passwords and the whole ledger of the previous
  owner stop working for it: sign-in and every session check require that device password,
  ledger and today's owner agree.

## Sessions

- A session token is 256 bit, created by the edge at sign-in, stored as SHA-256, valid for 12
  hours and for one mailbox. The bridge keeps it in the memory of one connection, never on disk
  and never in a log.
- The mailbox of a request comes from the token only. Request bodies cannot name a mailbox;
  unknown fields are rejected.
- Every request re-checks the token, the device password and your callbacks (`mailboxOwner`,
  `allowMember`) in the transaction that does the work, so a revocation cannot slip in between
  check and action.
- `/submit` is the one route whose work is done by your app, and done late: the edge checks the
  session, reads the message (up to 5 MiB) and then calls your `submit`, which may wait for a mail
  provider. The check at the start does not cover that time. The sentence above holds for
  `/submit` when the mutation of your app that records the message calls the `session` function
  with `submission.tokenHash`: the same check, in the transaction that writes. The example app
  does, and its test revokes a device password while the body is being read. An app that skips
  this step stores (and sends) a message for a session that ended while the message was on its
  way.

## The shared secret

- `MAIL_BRIDGE_SECRET`, at least 32 characters, set in the Convex deployment and in the bridge.
  Without it, or with a shorter one, every route answers `401` with an empty body before reading
  the request.
- It is compared as SHA-256 in constant time.
- What the secret alone allows: calling `/login` and `/seq`. It reads no mail; that needs a
  device password or a live session token. What it does protect: the edge is not an open
  endpoint, and the client IP in `/login` (which only the bridge can know) is trustworthy, which
  the throttle depends on.
- Rotate it by setting the new value in both places; the bridge needs a restart. Open
  connections fail and mail clients reconnect.

## Throttling failed sign-ins

- Counted per pair of address and client IP (10 failures in 15 minutes) and per client IP (30).
  IPv6 clients count by their /64.
- Keys are hashes over the shared secret, address and IP; the database holds neither addresses
  nor IPs.
- Only invalid attempts count. A valid device password always signs in, so a device that keeps
  retrying an old password cannot lock out a second device behind the same address.
- Unknown addresses go through the same steps as known ones and get the same answer: the same
  calls to the component in the same order, with an empty key where there is no mailbox. What
  your `mailboxByAddress` reads is up to you.
- The table of counters is capped; when it is full, further invalid attempts are refused without
  a write.

## What the bridge holds

In memory only, all bounded, all gone on restart:

- per mailbox with recent connections, a mirror of folder rows: UID, flags, size, date;
- recently fetched raw messages (default 64 MiB, least recently used first out);
- per connection, its session token;
- a short-lived note of a sign-in that was just throttled, as a keyed hash.

The device password is forwarded to `/login` and not kept. A connection may read mirror and
cache only for the mailbox it signed in to itself, and only while the edge confirmed its session
within the last few seconds (`SESSION_FRESH_SECONDS`). Logs and metrics contain no addresses,
subjects, contents, passwords, tokens or client addresses; mailboxes appear as truncated hashes.

## Who can read mail in transit

- **The operator of the TLS proxy.** TLS ends there; behind it device passwords and messages are
  plaintext up to the bridge. If that is a hosting provider, it is a processor of your users'
  mail. See [operations.md](operations.md#who-can-read-what).
- **The bridge host.** Same position.
- **Convex**, as for the rest of your app.

The bridge must only be reachable through the proxy, and it must only accept the PROXY protocol
header from the proxy (`PROXY_TRUSTED_CIDRS`). Otherwise someone who reaches the mail ports
directly could speak plaintext and fake a client address.

## What your app has to get right

The package cannot check these for you:

- **`rawLocation`** must verify that the message belongs to the mailbox it is asked for.
- **`mailboxOwner`** must return `null` for mailboxes that are deactivated.
- **`submit`** must check the sender. A signed-in device should only send as its own mailbox.
  It should also limit volume (see `allowSubmit`) and make retries harmless.
- **The mutation that records a submitted message** must ask the `session` function again and
  store nothing when it answers `null` (see "Sessions" and step 4 of
  [integration.md](integration.md#4-mount-the-routes)).
- **Creating device passwords** must be limited to people who may use the mailbox.
- **Profile files with a password** (`buildMailProfile` with `password`) are as sensitive as the
  password. Hand them out once, over a short-lived link.

## Known gaps

See "Hardening still open" in [limits.md](limits.md). In short: the bridge itself does not slow
down sign-in attempts (the edge counts and refuses them), an authenticated client can hold
working memory for a while, and `/metrics` has no authentication and relies on network
isolation.

## Reporting

See [SECURITY.md](../SECURITY.md).
