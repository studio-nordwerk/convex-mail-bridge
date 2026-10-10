# Connecting your app

This is the long version of the quick start in the README. The running example is the app in
[`example/convex`](../example/convex): two tables, about 400 lines, and a test suite that drives
every route. Copy from it freely.

## What you need

- A Convex app that stores messages and can hand out each message as **raw RFC 822 bytes**: the
  message exactly as it went over the wire. Mail clients download these bytes and parse them
  themselves. If you only keep parsed fields, you have to store the raw message too (Convex file
  storage is fine) or build it once and keep it. It must not change afterwards.
- A stable string key per mailbox and per message (their document ids), and a key for the owner
  of a mailbox (a user id).
- Somewhere to run the bridge: any place that can run a container behind a TLS proxy. See
  [operations.md](operations.md).

## The three kinds of code

| You write | It runs | Purpose |
| --- | --- | --- |
| calls to `bridge.place`, `bridge.setSeen`, `bridge.remove` in your own mutations | in your transactions | tell the ledger what happened to a message |
| callbacks passed to `bridge.edge({...})` | in the query or mutation behind a route | answer questions and react to changes made in a mail client |
| `submit` and `loadRaw` passed to `bridge.registerRoutes` | in the HTTP action | send mail, load bytes from somewhere other than Convex storage |

The component itself cannot read your tables and has no access to `ctx.auth` or your environment
variables. That is why the HTTP routes are part of the client layer and run in your app.

## Step by step

### 1. Mount the component

```ts
// convex/convex.config.ts
import { defineApp } from 'convex/server'
import mailBridge from '@nordwerk/convex-mail-bridge/convex.config.js'

const app = defineApp()
app.use(mailBridge)
export default app
```

### 2. Create the client and the edge functions

```ts
// convex/mailBridge.ts
import { MailBridge } from '@nordwerk/convex-mail-bridge'
import { components } from './_generated/api'
import type { DataModel } from './_generated/dataModel'

export const bridge = new MailBridge<DataModel>(components.mailBridge)

export const { login, seq, folders, entries, changes, raw, store, expunge, append, submitCheck, session, cleanup } =
  bridge.edge({
    mailboxByAddress,
    mailboxOwner,
    rawLocation,
    // optional: allowMember, onSeen, onExpunge, findSent, allowSubmit
  })
```

`bridge.edge` returns internal queries and mutations. They have to be exported from a module of
your app, all from the same one, under exactly these names. Nothing here is reachable from a
browser. The routes call all of them except two: `session` you call yourself when a mail client
sends a message (step 4), and `cleanup` you schedule (step 7).

Options of `new MailBridge(component, options)`:

| Option | Default | Meaning |
| --- | --- | --- |
| `secret` | `process.env.MAIL_BRIDGE_SECRET` | the shared secret, or a function returning it |
| `folderNames` | `INBOX`, `Sent`, `Archive`, `Trash`, `Later` | what folders are called in mail clients |
| `storeBatch` | `10` | UIDs per mutation when a mail client changes flags |
| `tombstoneRetentionMs` | 30 days | how long removals are remembered for mail clients that were offline |

### 3. The callbacks

All callbacks get the Convex `ctx` of the function they run in, typed with your data model.

**`mailboxByAddress(ctx, address)`** returns `{ mailboxKey, ownerKey }` or `null`. The address
arrives lowercased and trimmed; it is what the user typed as the user name. Return `null` for
mailboxes that must not be used. Do not throw for unknown addresses: the sign-in is then counted
as a failed attempt, at the same cost as a wrong password.

**`mailboxOwner(ctx, mailboxKey)`** returns the owner of the mailbox today, or `null` when it is
gone or switched off. It runs on every single request. A session only stays valid while this is
still the owner who called `enable`. This is what makes "transfer the mailbox to someone else" or
"deactivate the account" take effect at once.

**`rawLocation(ctx, { mailboxKey, messageKey, folder, ... })`** returns where the bytes of the
message are, or `null`. With the default loader that is an id of Convex file storage. **Check
here that the message belongs to `mailboxKey`**: the UID came from the session's mailbox, but it
is your table that knows whose message it is.

**`onSeen(ctx, { mailboxKey, folder, messages })`** (optional) is called when a mail client set
or cleared `\Seen`. It runs in the same mutation that updates the ledger. `messages` lists every
message the request named with the state it has now; write only where your data differs. Do not
call `bridge.setSeen` from here, the ledger is already up to date.

**`onExpunge(ctx, { mailboxKey, folder, messageKeys })`** (optional) is called when a mail client
deleted messages. By default the package only hides them from mail clients and your data stays.
Use this if a delete in the mail client should do something in your app.

**`findSent(ctx, { mailboxKey, messageId })`** (optional, but needed for sending) returns the
keys of sent messages with that Message-ID (without angle brackets). After sending, mail clients
store a copy in `Sent`. With this callback the edge answers "that message is already here, it is
UID 12". Without it the copy is refused and some clients show an error.

**`allowMember(ctx, { mailboxKey, ownerKey, userKey })`** (optional) makes shared mailboxes
possible: return `true` while this user, who is not the owner, may use the mailbox. It is asked
on every request made with one of that user's device passwords.

**`allowSubmit(ctx, session)`** (optional) runs before the body of a submission is read. Count
the attempt and return `false` to refuse it as rate limited. It is not called while
`registerRoutes` has no `submit`: without a way to send there is nothing to count.

### 4. Mount the routes

```ts
// convex/http.ts
import { httpRouter } from 'convex/server'
import { internal } from './_generated/api'
import { bridge } from './mailBridge'

const http = httpRouter()

bridge.registerRoutes(http, {
  functions: internal.mailBridge,
  submit: async (ctx, submission) => {
    // submission: { mailboxKey, ownerKey, userKey, tokenHash, envelope: { from, to }, raw, digest }
    // Check, record the message (asking `session` again), send with your provider, place it in Sent.
    return { status: 'sent', messageKey }
  },
})

export default http
```

This adds `POST /bridge/v1/{login,seq,folders,entries,changes,raw,store,expunge,append,submit}`
to your deployment's `.convex.site` URL. The paths are fixed; the bridge expects them there.

**`submit`** is where a message handed in over SMTP gets sent. The package does not send mail. A
careful `submit`:

1. checks that `envelope.from` and the `From` header are the address of the mailbox (otherwise a
   device could send under another name) and answers `{ status: 'rejected', reason: 'sender' }`;
2. checks the recipients against `To` and `Cc` if you care about Bcc (`reason: 'recipients'`);
3. looks up the Message-ID: if this exact message (same `digest`) was already accepted, it
   answers with that message again instead of sending twice; other bytes under the same id are a
   `conflict`. Mail clients retry when a connection breaks, so this matters;
4. records the message in a mutation that **checks the session again** (see below), before
   anything leaves; if the session has ended it answers `{ status: 'session' }`;
5. sends, and calls `bridge.place` with folder `Sent`;
6. returns `{ status: 'sent', messageKey }`. If the provider timed out and you cannot tell whether
   the message went out, return `{ status: 'unknown' }`: the mail client is told it was accepted
   and will not send it a second time.

The rejection reasons and what the mail client hears are listed in
[edge-contract.md](edge-contract.md#rejections).

**Checking the session again.** Every other route checks the session in the transaction that
does its work. For `/submit` the work is yours and happens late: the edge checks the session
(and asks `allowSubmit`), then reads up to 5 MiB, then calls `submit`, which may wait for a
provider. A device password revoked or a member removed in that time must not get a message
filed under the mailbox. So `submission.tokenHash` carries a handle to the session (the hash of
its token, not the token), and the `session` function from `bridge.edge` resolves it:

```ts
// convex/mail.ts: the mutation your `submit` calls to record the message
export const storeSubmitted = internalMutation({
  args: { tokenHash: v.string(), mailboxId: v.id('mailboxes') /* , the message */ },
  handler: async (ctx, args) => {
    const session = await ctx.runQuery(internal.mailBridge.session, {
      tokenHash: args.tokenHash,
      now: Date.now(),
    })
    // Gone, or not this mailbox: store nothing.
    if (!session || session.mailboxKey !== args.mailboxId) return null
    // insert the message, bridge.place(ctx, { ..., folder: 'Sent' })
  },
})
```

Called from a mutation, the query runs in that mutation's transaction, so the check and the
write cannot be separated. It is the same check as on every route (token, device password,
`mailboxOwner`, `allowMember`); it reads only and does not ask `allowSubmit` again. When it
returns `null`, have `submit` return `{ status: 'session' }`: the edge answers `401 session` and
the bridge ends the connection, as for any other request of a dead session. Do not store
`tokenHash`. If you send through a provider, do this in the step that records the message as
"being sent", before the call to the provider; what was handed to a provider cannot be taken
back.

**`loadRaw(ctx, location)`** (optional) loads bytes if they are not in Convex file storage. It
may return a `Blob`, an `ArrayBuffer` or a `Uint8Array`. Without it, `location` has to be an id
of Convex file storage; anything else is answered like a message without bytes (`404 gone`) and
logged as a warning, without the location. An error thrown by your own `loadRaw` is a `500`.

### 5. Feed the ledger

Wherever your app stores, reads or deletes a message, the same mutation tells the bridge. That
keeps numbering and your data in one transaction.

```ts
// once per mailbox, for example when it is created
await bridge.enable(ctx, { mailboxKey, ownerKey })

// a message arrived, or was sent
await bridge.place(ctx, {
  mailboxKey,
  messageKey,
  folder: 'INBOX', // 'Sent' for sent mail
  size,            // byte length of the raw message, exactly
  internalDate,    // milliseconds since 1970
  seen: false,
})

// read or unread in YOUR app
await bridge.setSeen(ctx, mailboxKey, [{ messageKey, seen: true }])

// deleted for good in your app: before you delete the document
await bridge.remove(ctx, messageKey)
```

`size` deserves care. Mail clients are told the size first and download the bytes later; if the
two differ, they misbehave in ways that are hard to debug. Take it from the stored file
(`(await ctx.db.system.get('_storage', id))?.size`) or from the bytes themselves, never from a
field you computed differently. The edge logs a warning when it notices a difference.

**A mailbox that already has messages.** Call `enable` with `backfill: true`, place the existing
messages in batches (a mutation that schedules itself until done), then call
`bridge.markReady(ctx, mailboxKey)`. Until then mail clients are told that the mailbox is being
prepared, so none of them ever sees a half-filled folder.

### 6. Device passwords

A mail client never gets the user's real password. It gets a device password your app creates:

```ts
export const createDevice = action({
  args: { mailboxId: v.id('mailboxes'), label: v.string() },
  handler: async (ctx, args) => {
    // 1. check that the signed-in user may use this mailbox
    const { password, secretHash } = await bridge.newDevicePassword()
    await ctx.runMutation(internal.mail.insertDevice, { ...args, secretHash })
    return { password } // show it once; it cannot be recovered
  },
})
```

It has to be an action, because generating the password needs cryptographic randomness. In the
mutation call `bridge.addDevice(ctx, { mailboxKey, ownerKey, label, secretHash })`. Also
available: `listDevices`, `revokeDevice`, `revokeMember`, `revokeAll`. Call `revokeAll` when you
switch a mailbox off.

What the user enters in the mail client:

| Field | Value |
| --- | --- |
| Account type | IMAP |
| User name (incoming and outgoing) | the full address |
| Password | the device password |
| Incoming server | your bridge host, port 993, TLS/SSL |
| Outgoing server | your bridge host, port 465, TLS/SSL |

For Apple devices, `buildMailProfile` creates a `.mobileconfig` file that fills all of this in.
Serve it with the media type `MAIL_PROFILE_MIME`. Without a password in it, the device asks for
the password once; with one, treat the file like the password itself. The component also has
the building blocks for a one-time set-up link that a phone can scan as a QR code
(`components.mailBridge.setup.*`: create, claim, redeem). They are tested but have no client
wrappers yet and count as experimental.

### 7. Housekeeping

```ts
// convex/crons.ts
crons.interval('clean up mail bridge', { hours: 1 }, internal.mailBridge.cleanup, {})
```

This removes expired sessions, old throttle windows, long-revoked device passwords and the
tombstones of deleted messages once they are older than `tombstoneRetentionMs`.

### 8. The secret and the bridge

```sh
npx convex env set MAIL_BRIDGE_SECRET "$(openssl rand -hex 32)"
```

Give the bridge the same value and your deployment's `.convex.site` URL. Continue with
[operations.md](operations.md).

## Testing your integration

Tests of an app that uses the component register it first:

```ts
import { convexTest } from 'convex-test'
import mailBridge from '@nordwerk/convex-mail-bridge/test'
import schema from './schema'

const t = convexTest(schema, import.meta.glob('./**/*.*s'))
mailBridge.register(t)
```

After that `t.fetch('/bridge/v1/login', ...)` goes through your real routes and callbacks.
[`example/convex/bridge.test.ts`](../example/convex/bridge.test.ts) shows the pattern for every
route, including the checks worth copying: another mailbox's rows are never visible, and a
session dies when the device password is revoked, the mailbox is switched off or its owner
changes.

To check a deployment from the outside, the bridge ships a probe that walks through IMAP against
your edge and reports what it finds: `bridge/test/against-edge.sh`, described in
[`bridge/README.md`](../bridge/README.md).

## Things that are your app's job

- Deciding who may create device passwords, and for which mailbox.
- Storing the raw message and reporting its exact size.
- Sending mail, including making a retry harmless and limiting how much a device may send.
- Checking the session again in the mutation that records a message a mail client sent.
- Whatever "delete" in a mail client should mean for your data.
- Receiving mail from the internet. The bridge does not accept inbound SMTP.
