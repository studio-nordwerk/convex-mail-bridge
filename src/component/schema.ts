import { defineSchema, defineTable } from 'convex/server'
import { v } from 'convex/values'

// Component `mailBridge`: what a mail client (IMAP) needs on top of the app's own store, namely a
// fixed running number per message and folder, plus the sign-in of mail clients (device
// passwords, session tokens, a throttle for failed attempts). The component knows nothing about
// threads, labels, organizations or users of the app. Every key that crosses the boundary is a
// string: `mailboxKey` (mailbox), `messageKey` (message), `ownerKey` (owner). Access is checked by
// the app. Secrets (device password, token) only ever arrive as SHA-256, never in clear.

/**
 * Folders of the ledger. Enabling a mailbox creates `INBOX` and `Sent`, the others only come with
 * `mailbox.addFolders`. The names are keys; what a folder is called in the mail client is decided
 * by the app.
 */
export const folderValidator = v.union(
  v.literal('INBOX'),
  v.literal('Sent'),
  v.literal('Archive'),
  v.literal('Trash'),
  v.literal('Later'),
)

/**
 * Kind of tombstone: `client` = removed in the mail client (the message still exists), `purge` =
 * the message is deleted for good, `move` = the message now lies in another folder (or came back
 * to this one with a new number).
 */
export const removedByValidator = v.union(
  v.literal('client'),
  v.literal('purge'),
  v.literal('move'),
)

/** Kind of a row for moving by group; it has no meaning for the component. */
export const kindValidator = v.union(v.literal('in'), v.literal('out'))

export const backfillStatusValidator = v.union(v.literal('running'), v.literal('done'))

/** Set-up link: open → with a download ticket → profile loaded (device created). */
export const setupStateValidator = v.union(
  v.literal('open'),
  v.literal('claimed'),
  v.literal('downloaded'),
)

export const tables = {
  // One document per mailbox and folder. Every change in the folder writes it (number, counters).
  folders: defineTable({
    mailboxKey: v.string(),
    folder: folderValidator,
    /** Seconds since 1970 at creation. Only changes with an explicit renumbering. */
    uidValidity: v.number(),
    /** Next free number. Only rises; numbers are never reused. */
    uidNext: v.number(),
    /** Highest change state assigned in the folder. */
    highestModseq: v.number(),
    /** Live rows. */
    exists: v.number(),
    /** Live rows without `\Seen`. */
    unseen: v.number(),
    /**
     * Highest change state of a removed row that cleanup has already deleted (missing = 0). A
     * mirror with an older state never saw that removal and can no longer learn of it:
     * `entries.changes` tells it `resync`.
     */
    resyncBelowModseq: v.optional(v.number()),
  }).index('by_mailboxKey_and_folder', ['mailboxKey', 'folder']),

  // Exactly one CURRENT row per message: the live one, otherwise the tombstone `client` or
  // `purge`. When the message changes folder it gets a new row with a new number there, and the
  // old one becomes a tombstone `move`; a message can have several of those. A number never comes
  // back to life.
  entries: defineTable({
    mailboxKey: v.string(),
    folder: folderValidator,
    uid: v.number(),
    messageKey: v.string(),
    /** Change state of this row: its own value per change, unique in the folder. */
    modseq: v.number(),
    /** Byte size of the raw message. */
    size: v.number(),
    internalDate: v.number(),
    /** Sorted and unique; `\Seen` is part of it. */
    flags: v.array(v.string()),
    live: v.boolean(),
    removedAt: v.optional(v.number()),
    removedModseq: v.optional(v.number()),
    removedBy: v.optional(removedByValidator),
    /**
     * Group and kind of the message, both assigned by the app and opaque here (for example: the
     * thread, and received or sent). With them `entries.moveGroup` moves all messages of a group
     * without the app having to read its messages.
     */
    groupKey: v.optional(v.string()),
    kind: v.optional(kindValidator),
  })
    // Rows of a message by kind: the current one has no `removedBy`, or `client` or `purge`.
    .index('by_messageKey_and_removedBy', ['messageKey', 'removedBy'])
    // Current rows of a group per kind and folder: `entries.moveGroup` only reads the rows that
    // are not in the target folder.
    .index('by_mailboxKey_and_groupKey_and_kind_and_removedBy_and_folder', [
      'mailboxKey',
      'groupKey',
      'kind',
      'removedBy',
      'folder',
    ])
    // Live rows of a folder by number, without reading the removed ones.
    .index('by_mailboxKey_and_folder_and_live_and_uid', ['mailboxKey', 'folder', 'live', 'uid'])
    // Changes of a folder since a state (removed rows included).
    .index('by_mailboxKey_and_folder_and_modseq', ['mailboxKey', 'folder', 'modseq'])
    // Cleaning up removed rows by time.
    .index('by_removedBy_and_removedAt', ['removedBy', 'removedAt']),

  // One row per mailbox: change counter and state of the backfill. Deliberately its own row and
  // never a field on the app's mailbox, so that the app's queries do not rerun with every message.
  state: defineTable({
    mailboxKey: v.string(),
    ownerKey: v.string(),
    enabledAt: v.number(),
    /** Rises with every call that changed something. */
    seq: v.number(),
    backfillStatus: backfillStatusValidator,
    /** Folder the backfill is working on. */
    backfillFolder: v.optional(folderValidator),
    /** Cursor of the app for the next batch (opaque for the component). */
    backfillCursor: v.optional(v.string()),
    /** Counts every start: only the latest run may keep writing (no parallel batches). */
    backfillRun: v.number(),
    backfillUpdatedAt: v.number(),
    /**
     * Bookmark of the app for its own repair run (opaque for the component): the last run tried
     * candidates up to here, the next one continues behind it.
     */
    repairCursor: v.optional(v.string()),
  }).index('by_mailboxKey', ['mailboxKey']),

  // Device password of a mailbox: the password ONE mail client signs in with. Only its SHA-256 is
  // stored. Revoked rows stay until cleanup (`revokedAt`).
  devicePasswords: defineTable({
    mailboxKey: v.string(),
    /** Owner at creation. If the mailbox changes its owner, the password is no longer valid. */
    ownerKey: v.string(),
    /**
     * Shared mailboxes: the person this device password belongs to when it is not the owner (a
     * member of the mailbox). Missing = a device of the owner. Whether that person may still use
     * the mailbox today is checked by the app on every call.
     */
    userKey: v.optional(v.string()),
    label: v.string(),
    secretHash: v.string(),
    createdAt: v.number(),
    /** Last successful sign-in, updated at most once per hour. */
    lastUsedAt: v.optional(v.number()),
    revokedAt: v.optional(v.number()),
  })
    // Sign-in: the password of this mailbox.
    .index('by_mailboxKey_and_secretHash', ['mailboxKey', 'secretHash'])
    // Active passwords of a mailbox (`revokedAt` missing), without reading the revoked ones.
    .index('by_mailboxKey_and_revokedAt', ['mailboxKey', 'revokedAt'])
    // Active passwords of one person at a mailbox (`userKey` missing = the owner).
    .index('by_mailboxKey_and_userKey_and_revokedAt', ['mailboxKey', 'userKey', 'revokedAt'])
    // Cleaning up revoked passwords by time.
    .index('by_revokedAt', ['revokedAt']),

  // Session token: after sign-in, for exactly one mailbox, bound to the device password. Only the
  // SHA-256 of the token is stored. Every check reads the device password too.
  sessions: defineTable({
    tokenHash: v.string(),
    mailboxKey: v.string(),
    devicePasswordId: v.id('devicePasswords'),
    createdAt: v.number(),
    expiresAt: v.number(),
    /**
     * Number of the backfill (`state.backfillRun`) at sign-in. When a new backfill starts
     * (switching to folders), the token is no longer valid. Missing on tokens from before this
     * rule.
     */
    run: v.optional(v.number()),
  })
    .index('by_tokenHash', ['tokenHash'])
    // Revoking a device password deletes its tokens.
    .index('by_devicePasswordId', ['devicePasswordId'])
    // Cleaning up expired tokens.
    .index('by_expiresAt', ['expiresAt']),

  // Set-up link (set-up by QR code): redeemable once, valid briefly. Redeeming trades it for a
  // download ticket; only redeeming the ticket creates the device password, in the same
  // transaction. Only the SHA-256 of link and ticket are stored, never a password.
  setupLinks: defineTable({
    mailboxKey: v.string(),
    /** Owner at creation. If the mailbox changes its owner, the link leads nowhere. */
    ownerKey: v.string(),
    /** As with `devicePasswords`: the member who created the link (missing = the owner). */
    userKey: v.optional(v.string()),
    /** Label of the device that is created when the profile is loaded. */
    label: v.string(),
    tokenHash: v.string(),
    createdAt: v.number(),
    /** `open`: expiry of the link. `claimed`: expiry of the ticket. After that nothing comes of it. */
    expiresAt: v.number(),
    state: setupStateValidator,
    ticketHash: v.optional(v.string()),
    claimedAt: v.optional(v.number()),
    downloadedAt: v.optional(v.number()),
    /** The device password created when the profile was loaded. */
    deviceId: v.optional(v.id('devicePasswords')),
  })
    .index('by_tokenHash', ['tokenHash'])
    .index('by_ticketHash', ['ticketHash'])
    // Links of a mailbox that can still become something (limit per mailbox).
    .index('by_mailboxKey_and_state_and_expiresAt', ['mailboxKey', 'state', 'expiresAt'])
    // Cleaning up expired links by time.
    .index('by_expiresAt', ['expiresAt']),

  // Throttle: failed attempts per key in a fixed time window. The app builds the key (a hash,
  // never an address or IP in clear); it also counts for unknown addresses. Only invalid attempts
  // are counted; a password or its hash is never stored here.
  loginAttempts: defineTable({
    key: v.string(),
    /** Time of the first failed attempt in this window. */
    windowStart: v.number(),
    count: v.number(),
  })
    .index('by_key', ['key'])
    // Cleaning up expired windows.
    .index('by_windowStart', ['windowStart']),

  // Number of rows in `loginAttempts` (exactly one row). A cap against a table that fills up:
  // once it is reached, no new row is created. Cleanup recounts.
  loginAttemptTotals: defineTable({ rows: v.number() }),
}

export default defineSchema(tables)
