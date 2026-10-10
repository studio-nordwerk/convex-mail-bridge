// Client layer of the mail bridge component. Everything here runs in YOUR app, not inside the
// component: it can read your tables and your environment variables, and it is where your app
// tells the bridge which mailbox an address belongs to, where the bytes of a message are, and what
// a change made in a mail client means for your data.
//
// Three parts:
//
//   1. Write path. Your mutations call `place`, `setSeen` and `remove` whenever a message arrives,
//      is read or is deleted in your app. The component assigns IMAP UIDs and keeps the counters.
//   2. Edge functions. `edge({ ...callbacks })` returns the internal functions behind the HTTP
//      routes. Export them from one module of your app.
//   3. Routes. `registerRoutes(http, { functions: internal.thatModule })` mounts `/bridge/v1/*`.
//
// Every edge function checks the session token, the device password behind it and your access
// callbacks in the same transaction as its work. The mailbox always comes from the session token,
// never from a request body. The one route whose work is yours is `/submit`: there the mutation of
// YOUR app that records the message repeats the check with the `session` function of `edge()`.

import {
  httpActionGeneric,
  internalMutationGeneric,
  internalQueryGeneric,
  type FunctionReference,
  type GenericActionCtx,
  type GenericDataModel,
  type GenericMutationCtx,
  type GenericQueryCtx,
  type HttpRouter,
} from 'convex/server'
import { v, type GenericId, type Infer } from 'convex/values'
import type { ComponentApi } from '../component/_generated/component.js'
import {
  BRIDGE_BASE,
  BRIDGE_LIMITS,
  BRIDGE_VERSION_FOLDERS,
  DEFAULT_FOLDER_NAMES,
  FOLDER_KEYS,
  bridgeAuthorized,
  bridgeError,
  bridgeJson,
  bridgeSubmitRejection,
  bridgeUnauthorized,
  folderKey,
  inBatches,
  isExtraFolder,
  isTokenShape,
  namesSeenFlag,
  newDevicePassword,
  newToken,
  parseAppend,
  parseChanges,
  parseEmpty,
  parseEntries,
  parseEnvelope,
  parseExpunge,
  parseLogin,
  parseRaw,
  parseSeq,
  parseStore,
  readJsonObject,
  readLimitedBody,
  secretHash,
  sha256Hex,
  throttleKeys,
  usableSecret,
  type Folder,
  type FolderName,
  type RawBytes,
  type RequestProblem,
  type SubmitEnvelope,
  type SubmitRejection,
} from './edge.js'

export type { ComponentApi } from '../component/_generated/component.js'
export {
  BRIDGE_BASE,
  BRIDGE_LIMITS,
  BRIDGE_PORTS,
  DEFAULT_FOLDER_NAMES,
  MIN_BRIDGE_SECRET_CHARS,
  SUBMIT_REJECTIONS,
  newDevicePassword,
  newToken,
  normalizeAddress,
  normalizeMessageId,
  secretHash,
  type Folder,
  type FolderName,
  type RawBytes,
  type SubmitEnvelope,
  type SubmitRejection,
} from './edge.js'
export {
  MAIL_PROFILE_MIME,
  buildMailProfile,
  mailProfileFilename,
  type MailProfileInput,
} from './mobileconfig.js'

// ─── Types ──────────────────────────────────────────────────────────────

type RunQueryCtx = Pick<GenericQueryCtx<GenericDataModel>, 'runQuery'>
type RunMutationCtx = Pick<GenericMutationCtx<GenericDataModel>, 'runQuery' | 'runMutation'>

/** A mailbox as your app names it. Both keys are opaque strings for the component. */
export type MailboxRef = {
  /** Stable key of the mailbox, for example the id of its document. */
  mailboxKey: string
  /** Who owns the mailbox today, for example a user id. */
  ownerKey: string
}

/** Who a request acts for: the mailbox of the session token and the human behind the device. */
export type BridgeSession = MailboxRef & {
  /** Owner of the device password: the owner of the mailbox or, for shared mailboxes, a member. */
  userKey: string
}

export type MailBridgeOptions = {
  /**
   * The shared secret of bridge and edge, or a function that returns it. Default: the environment
   * variable `MAIL_BRIDGE_SECRET` of your deployment. Shorter than 32 characters counts as unset,
   * and without a secret every route answers 401.
   */
  secret?: string | (() => string | undefined)
  /** Names of the folders in the mail client. Default: `DEFAULT_FOLDER_NAMES`. */
  folderNames?: Partial<Record<Folder, FolderName>>
  /**
   * UIDs per mutation of `/store` (1 to 500, default 10). Each batch runs `onSeen` once; keep it
   * small if that callback reads whole message documents.
   */
  storeBatch?: number
  /** How long tombstones of deleted messages are kept before `cleanup` removes them. 30 days. */
  tombstoneRetentionMs?: number
}

export type EdgeCallbacks<DataModel extends GenericDataModel> = {
  /**
   * The mailbox a mail client signs in to with this address (lowercased, trimmed), or `null` if
   * there is none or it must not be used. Unknown addresses cost the same work as known ones and
   * count as failed attempts.
   */
  mailboxByAddress: (ctx: GenericQueryCtx<DataModel>, address: string) => Promise<MailboxRef | null>
  /**
   * Who owns this mailbox today, or `null` if it no longer exists or is switched off. Called on
   * EVERY request. A session is only valid while this is the owner who enabled the mailbox: after
   * a transfer the new owner sees neither counters nor devices of the previous one.
   */
  mailboxOwner: (ctx: GenericQueryCtx<DataModel>, mailboxKey: string) => Promise<string | null>
  /**
   * Shared mailboxes only: may this user, who is not the owner, still use the mailbox? Called on
   * every request made with a device password of that user. Default: never.
   */
  allowMember?: (
    ctx: GenericQueryCtx<DataModel>,
    member: { mailboxKey: string; ownerKey: string; userKey: string },
  ) => Promise<boolean>
  /**
   * Where the raw message (RFC 822 bytes) of `messageKey` is stored, or `null` if it is gone. Check
   * here that the message belongs to this mailbox. The default loader of `registerRoutes` expects
   * an id of Convex file storage; with your own `loadRaw` it can be any string.
   */
  rawLocation: (
    ctx: GenericQueryCtx<DataModel>,
    message: BridgeSession & { messageKey: string; folder: Folder },
  ) => Promise<string | null>
  /**
   * A mail client marked messages as read or unread. Runs in the same mutation as the ledger, so
   * both commit or neither does. The ledger is already updated: do NOT call `setSeen` here. Lists
   * every message the request named, also those whose flag did not change, with the state they
   * have now; write only where your data differs.
   */
  onSeen?: (
    ctx: GenericMutationCtx<DataModel>,
    change: BridgeSession & { folder: Folder; messages: Array<{ messageKey: string; seen: boolean }> },
  ) => Promise<void>
  /**
   * A mail client deleted messages (EXPUNGE). The package only hides them from mail clients; what
   * that means for your data is your decision. Same mutation as the ledger.
   */
  onExpunge?: (
    ctx: GenericMutationCtx<DataModel>,
    removed: BridgeSession & { folder: Folder; messageKeys: string[] },
  ) => Promise<void>
  /**
   * Keys of sent messages of this mailbox with this Message-ID (without angle brackets). Mail
   * clients store a copy of what they sent in `Sent`; with this the edge answers with the message
   * that is already there instead of a duplicate. Without it such a copy is refused.
   */
  findSent?: (
    ctx: GenericQueryCtx<DataModel>,
    lookup: BridgeSession & { messageId: string },
  ) => Promise<string[]>
  /**
   * Called before the body of a submission is read. Return `false` to refuse it as rate limited.
   * This is the place to count attempts. Not called while `registerRoutes` has no `submit`.
   */
  allowSubmit?: (ctx: GenericMutationCtx<DataModel>, session: BridgeSession) => Promise<boolean>
}

/** A message a mail client hands in over SMTP. */
export type Submission = BridgeSession & {
  /**
   * Handle of the session the message came in on: the SHA-256 of its token, never the token. The
   * session was valid before the body was read, and a body takes time. Pass this to the `session`
   * function of `edge()` in the mutation that records the message, and answer
   * `{ status: 'session' }` when it returns `null`. Do not store it.
   */
  tokenHash: string
  /** Envelope sender and recipients (lowercased). Check them against the mailbox and the headers. */
  envelope: SubmitEnvelope
  /** The message exactly as the mail client sent it, at most 5 MiB. */
  raw: RawBytes
  /** SHA-256 of `raw`, lowercase hex. */
  digest: string
}

export type SubmitResult =
  /** Sent. With `messageKey` (already placed in `Sent`) the mail client learns its UID. */
  | { status: 'sent'; messageKey?: string }
  /** The outcome is not known (provider timeout). The mail client is told it was accepted. */
  | { status: 'unknown' }
  | { status: 'rejected'; reason: SubmitRejection; message?: string }
  /**
   * The session ended while the message was on its way (device password revoked, member removed,
   * mailbox switched off): the `session` function returned `null` in the mutation that would have
   * recorded it. Answer this only when nothing was sent or stored; the route answers `401 session`.
   */
  | { status: 'session' }

type RawBody = Blob | ArrayBuffer | Uint8Array

export type RouteOptions<DataModel extends GenericDataModel> = {
  /** The module that exports the result of `edge()`, for example `internal.mailBridge`. */
  functions: EdgeFunctionReferences
  /**
   * Loads the bytes for a location returned by `rawLocation`. Default: `ctx.storage.get(location)`;
   * a location that is not an id of Convex file storage is answered as a missing message. A
   * storage URL never leaves the edge.
   */
  loadRaw?: (ctx: GenericActionCtx<DataModel>, location: string) => Promise<RawBody | null>
  /**
   * Sends a message handed in over SMTP with the provider your app uses. Must be safe to call
   * twice for the same message: a mail client retries when the connection breaks. It runs after
   * the body was read, so the mutation that records the message has to check the session again
   * (`Submission.tokenHash`). Without `submit` mail clients can read but not send.
   */
  submit?: (ctx: GenericActionCtx<DataModel>, submission: Submission) => Promise<SubmitResult>
}

// ─── Validators of the edge functions ───────────────────────────────────

const sessionArgs = { tokenHash: v.string(), now: v.number() }
const noSession = v.object({ kind: v.literal('session') })
const noFolder = v.object({ kind: v.literal('folder') })
const NO_SESSION = { kind: 'session' as const }
const NO_FOLDER = { kind: 'folder' as const }

const folderRow = v.object({
  name: v.string(),
  specialUse: v.optional(v.string()),
  uidValidity: v.number(),
  uidNext: v.number(),
  highestModseq: v.number(),
  exists: v.number(),
  unseen: v.number(),
})

/** What leaves the edge of a row. The key of the message (`messageKey`) never does. */
const entryRow = {
  uid: v.number(),
  flags: v.array(v.string()),
  modseq: v.number(),
  size: v.number(),
  internalDate: v.number(),
}

const folderState = { uidValidity: v.number(), uidNext: v.number(), highestModseq: v.number() }

const loginArgs = v.object({
  address: v.string(),
  secretHash: v.string(),
  tokenHash: v.string(),
  pairKey: v.string(),
  ipKey: v.string(),
  bridge: v.optional(v.number()),
})
const loginResult = v.union(
  v.object({ kind: v.literal('ok'), expiresAt: v.number(), mailbox: v.string() }),
  v.object({ kind: v.literal('auth') }),
  v.object({ kind: v.literal('preparing') }),
  v.object({ kind: v.literal('throttled'), retryAfterSeconds: v.number() }),
)

const seqArgs = v.object({ tokenHashes: v.array(v.string()), now: v.number() })
const seqResult = v.array(
  v.union(v.object({ ok: v.literal(true), seq: v.number() }), v.object({ ok: v.literal(false) })),
)

const foldersArgs = v.object(sessionArgs)
const foldersResult = v.union(
  noSession,
  v.object({ kind: v.literal('ok'), folders: v.array(folderRow) }),
)

const entriesArgs = v.object({
  ...sessionArgs,
  folder: v.string(),
  afterUid: v.optional(v.number()),
  limit: v.number(),
})
const entriesResult = v.union(
  noSession,
  noFolder,
  v.object({
    kind: v.literal('ok'),
    rows: v.array(v.object(entryRow)),
    more: v.boolean(),
    ...folderState,
  }),
)

const changesArgs = v.object({
  ...sessionArgs,
  folder: v.string(),
  sinceModseq: v.number(),
  limit: v.number(),
})
const changesResult = v.union(
  noSession,
  noFolder,
  v.object({ kind: v.literal('resync') }),
  v.object({
    kind: v.literal('ok'),
    rows: v.array(v.object({ ...entryRow, live: v.boolean() })),
    more: v.boolean(),
    ...folderState,
  }),
)

const rawArgs = v.object({
  ...sessionArgs,
  folder: v.string(),
  uid: v.number(),
  uidValidity: v.number(),
})
const rawResult = v.union(
  noSession,
  v.object({ kind: v.literal('gone') }),
  v.object({ kind: v.literal('uidvalidity') }),
  v.object({ kind: v.literal('ok'), location: v.string(), size: v.number() }),
)

const storeArgs = v.object({
  tokenHash: v.string(),
  folder: v.string(),
  uids: v.array(v.number()),
  add: v.array(v.string()),
  remove: v.array(v.string()),
})
const storeResult = v.union(
  noSession,
  noFolder,
  v.object({
    kind: v.literal('ok'),
    rows: v.array(v.object({ uid: v.number(), flags: v.array(v.string()), modseq: v.number() })),
  }),
)

const expungeArgs = v.object({
  tokenHash: v.string(),
  folder: v.string(),
  uids: v.array(v.number()),
})
const expungeResult = v.union(
  noSession,
  noFolder,
  v.object({ kind: v.literal('ok'), uids: v.array(v.number()) }),
)

const appendArgs = v.object({
  ...sessionArgs,
  folder: v.string(),
  messageId: v.string(),
  digest: v.string(),
})
const appendResult = v.union(
  noSession,
  noFolder,
  v.object({ kind: v.literal('unknown') }),
  v.object({
    kind: v.literal('ok'),
    /** Both missing: the message exists, but not in `Sent`. */
    uid: v.optional(v.number()),
    uidValidity: v.optional(v.number()),
  }),
)

const submitCheckArgs = v.object({ tokenHash: v.string() })
const submitCheckResult = v.union(
  noSession,
  v.object({ kind: v.literal('limit') }),
  v.object({
    kind: v.literal('ok'),
    mailboxKey: v.string(),
    ownerKey: v.string(),
    userKey: v.string(),
  }),
)

const sessionCheckArgs = v.object(sessionArgs)
const sessionCheckResult = v.union(
  v.null(),
  v.object({ mailboxKey: v.string(), ownerKey: v.string(), userKey: v.string() }),
)

const cleanupResult = v.object({
  sessions: v.number(),
  attempts: v.number(),
  devices: v.number(),
  setupLinks: v.number(),
  tombstones: v.number(),
  isDone: v.boolean(),
})

type Ref<
  Kind extends 'query' | 'mutation',
  Args extends Record<string, unknown>,
  Result,
> = FunctionReference<Kind, 'internal', Args, Result>

/** References to the functions `edge()` returns, as they appear under `internal.<module>`. */
export type EdgeFunctionReferences = {
  login: Ref<'mutation', Infer<typeof loginArgs>, Infer<typeof loginResult>>
  seq: Ref<'query', Infer<typeof seqArgs>, Infer<typeof seqResult>>
  folders: Ref<'query', Infer<typeof foldersArgs>, Infer<typeof foldersResult>>
  entries: Ref<'query', Infer<typeof entriesArgs>, Infer<typeof entriesResult>>
  changes: Ref<'query', Infer<typeof changesArgs>, Infer<typeof changesResult>>
  raw: Ref<'query', Infer<typeof rawArgs>, Infer<typeof rawResult>>
  store: Ref<'mutation', Infer<typeof storeArgs>, Infer<typeof storeResult>>
  expunge: Ref<'mutation', Infer<typeof expungeArgs>, Infer<typeof expungeResult>>
  append: Ref<'query', Infer<typeof appendArgs>, Infer<typeof appendResult>>
  submitCheck: Ref<'mutation', Infer<typeof submitCheckArgs>, Infer<typeof submitCheckResult>>
  session: Ref<'query', Infer<typeof sessionCheckArgs>, Infer<typeof sessionCheckResult>>
}

const DAY_MS = 24 * 60 * 60 * 1000
const SEEN = '\\Seen'
/** More messages with the same Message-ID are not looked at (a mail to oneself: two). */
const APPEND_CANDIDATES = 10
const CLEANUP_BATCH = 500

// ─── The client ─────────────────────────────────────────────────────────

export class MailBridge<DataModel extends GenericDataModel = GenericDataModel> {
  readonly folderNames: Record<Folder, FolderName>

  constructor(
    public component: ComponentApi,
    public options: MailBridgeOptions = {},
  ) {
    this.folderNames = { ...DEFAULT_FOLDER_NAMES, ...options.folderNames }
    const names = FOLDER_KEYS.map((key) => this.folderNames[key].name.normalize('NFC'))
    if (new Set(names).size !== names.length || this.folderNames.INBOX.name !== 'INBOX') {
      throw new Error('mailBridge: folder names must be unique and INBOX must be called INBOX')
    }
    const batch = options.storeBatch
    if (batch !== undefined && (!Number.isInteger(batch) || batch < 1 || batch > BRIDGE_LIMITS.uids)) {
      throw new Error('mailBridge: storeBatch must be an integer from 1 to 500')
    }
  }

  private secret(): string | null {
    const option = this.options.secret
    const value = typeof option === 'function' ? option() : (option ?? process.env.MAIL_BRIDGE_SECRET)
    return usableSecret(value)
  }

  /** Is a usable shared secret set? Without one every route answers 401. */
  configured(): boolean {
    return this.secret() !== null
  }

  // ─── Mailbox ──────────────────────────────────────────────────────────

  /**
   * Starts numbering for a mailbox: creates `INBOX` and `Sent`. Idempotent; a second call changes
   * nothing, not even the owner.
   *
   * With `backfill: true` mail clients are told "being prepared" until you call `markReady`: use it
   * when the mailbox already holds messages and you `place` them in several batches. Without it
   * the mailbox is usable at once.
   */
  async enable(ctx: RunMutationCtx, mailbox: MailboxRef & { backfill?: boolean }) {
    const result = await ctx.runMutation(this.component.mailbox.enable, {
      mailboxKey: mailbox.mailboxKey,
      ownerKey: mailbox.ownerKey,
    })
    if (result.created && mailbox.backfill !== true) await this.markReady(ctx, mailbox.mailboxKey)
    return result
  }

  /** The existing messages are placed: mail clients may sign in. `false` = mailbox not enabled. */
  async markReady(ctx: RunMutationCtx, mailboxKey: string): Promise<boolean> {
    return await ctx.runMutation(this.component.mailbox.setBackfill, { mailboxKey, status: 'done' })
  }

  /** State of a mailbox (owner, change counter, backfill), or `null` if it is not enabled. */
  async status(ctx: RunQueryCtx, mailboxKey: string) {
    return await ctx.runQuery(this.component.mailbox.get, { mailboxKey })
  }

  /** The folders of a mailbox with their UID and counter state. */
  async folders(ctx: RunQueryCtx, mailboxKey: string) {
    return await ctx.runQuery(this.component.folders.list, { mailboxKey })
  }

  // ─── Write path: your app tells the ledger what happened ──────────────

  /**
   * Gives a message its UID in a folder. Call it in the mutation that stores the message, for
   * incoming mail (`INBOX`) and for sent mail (`Sent`). Idempotent in the same folder; in another
   * folder the message moves there and gets a new UID. `null` = the mailbox is not enabled.
   *
   * `size` MUST be the exact byte length of the raw message that `/raw` will deliver: mail clients
   * compare it. `internalDate` is in milliseconds since 1970.
   */
  async place(
    ctx: RunMutationCtx,
    message: {
      mailboxKey: string
      messageKey: string
      folder: Folder
      size: number
      internalDate: number
      seen: boolean
      flags?: string[]
      /** Opaque group (for example a thread) and kind, for `moveGroup`. */
      groupKey?: string
      kind?: 'in' | 'out'
    },
  ) {
    return await ctx.runMutation(this.component.entries.add, message)
  }

  /** Messages were read or marked unread in YOUR app. Returns how many rows changed. */
  async setSeen(
    ctx: RunMutationCtx,
    mailboxKey: string,
    changes: Array<{ messageKey: string; seen: boolean }>,
  ): Promise<number> {
    if (changes.length === 0) return 0
    return await ctx.runMutation(this.component.entries.setSeenMany, { mailboxKey, changes })
  }

  /**
   * The message is deleted for good in your app. Call it BEFORE you delete the document. Its row
   * stays as a tombstone until `cleanup` removes it, so that mail clients learn about the removal.
   */
  async remove(ctx: RunMutationCtx, messageKey: string): Promise<boolean> {
    return await ctx.runMutation(this.component.entries.remove, { messageKey })
  }

  /** The current row of a message (folder, UID, flags), or `null`. */
  async entry(ctx: RunQueryCtx, messageKey: string) {
    return await ctx.runQuery(this.component.entries.byMessage, { messageKey })
  }

  /**
   * Experimental (folders, contract revision 1.2): creates further folders for an enabled mailbox.
   * The bridge in this repository does not serve such a mailbox yet.
   */
  async addFolders(ctx: RunMutationCtx, mailboxKey: string, folders: Folder[]) {
    return await ctx.runMutation(this.component.mailbox.addFolders, { mailboxKey, folders })
  }

  /** Experimental (folders): moves all messages of a group to the folder for their kind. */
  async moveGroup(
    ctx: RunMutationCtx,
    move: {
      mailboxKey: string
      groupKey: string
      targets: { in?: Folder; out?: Folder }
      limit?: number
    },
  ) {
    return await ctx.runMutation(this.component.entries.moveGroup, {
      mailboxKey: move.mailboxKey,
      groupKey: move.groupKey,
      targets: move.targets,
      limit: move.limit ?? 200,
    })
  }

  // ─── Device passwords ─────────────────────────────────────────────────

  /**
   * A new device password and its hash. Call it in an ACTION (it needs cryptographic randomness),
   * show `password` to the user once and store only `secretHash` with `addDevice`.
   */
  async newDevicePassword(prefix?: string): Promise<{ password: string; secretHash: string }> {
    const password = newDevicePassword(prefix)
    return { password, secretHash: await secretHash(password) }
  }

  /**
   * Stores a device password (its hash) for a mailbox. Check first that the caller may use the
   * mailbox. `userKey`: for shared mailboxes the member the device belongs to; omit it for the
   * owner. Results: `created`, `limit` (10 active per person and mailbox), `disabled` (mailbox not
   * enabled), `owner` (the mailbox was enabled by another owner).
   */
  async addDevice(
    ctx: RunMutationCtx,
    device: MailboxRef & { userKey?: string; label: string; secretHash: string },
  ) {
    return await ctx.runMutation(this.component.devices.create, device)
  }

  /** Active device passwords of one person at a mailbox, without hashes. */
  async listDevices(ctx: RunQueryCtx, mailboxKey: string, userKey?: string) {
    return await ctx.runQuery(this.component.devices.list, {
      mailboxKey,
      ...(userKey !== undefined ? { userKey } : {}),
    })
  }

  /** Revokes one device password and its sessions. Open connections end within seconds. */
  async revokeDevice(
    ctx: RunMutationCtx,
    device: { mailboxKey: string; id: string; userKey?: string },
  ): Promise<boolean> {
    return await ctx.runMutation(this.component.devices.revoke, device)
  }

  /** Revokes every device password of a member (shared mailboxes), with sessions and setup links. */
  async revokeMember(ctx: RunMutationCtx, member: { mailboxKey: string; userKey: string }) {
    return await ctx.runMutation(this.component.devices.revokeUser, member)
  }

  /** Revokes every device password of a mailbox. Call it when you switch a mailbox off. */
  async revokeAll(ctx: RunMutationCtx, mailboxKey: string) {
    return await ctx.runMutation(this.component.devices.revokeAll, { mailboxKey })
  }

  // ─── Maintenance ──────────────────────────────────────────────────────

  /**
   * Deletes expired sessions, old throttle windows, long revoked device passwords, expired setup
   * links and tombstones past their retention. One bounded batch per call; `isDone: false` means
   * there is more. `edge()` returns this as a function you can schedule hourly.
   */
  async cleanup(ctx: RunMutationCtx, now: number = Date.now()) {
    const access = await ctx.runMutation(this.component.sessions.cleanup, {
      now,
      limit: CLEANUP_BATCH,
    })
    const removed = await ctx.runMutation(this.component.maintenance.cleanup, {
      before: now - (this.options.tombstoneRetentionMs ?? 30 * DAY_MS),
      limit: CLEANUP_BATCH,
    })
    return {
      sessions: access.sessions,
      attempts: access.attempts,
      devices: access.devices,
      setupLinks: access.setupLinks,
      tombstones: removed.deleted,
      isDone: access.isDone && removed.isDone,
    }
  }

  // ─── Edge functions ───────────────────────────────────────────────────

  /**
   * The functions behind the routes of `/bridge/v1`, one per route, plus `session` and `cleanup`.
   * Export all of them from one module of your app and pass that module to `registerRoutes`:
   *
   * ```ts
   * export const { login, seq, folders, entries, changes, raw, store, expunge, append,
   *   submitCheck, session, cleanup } = bridge.edge({ mailboxByAddress, mailboxOwner, rawLocation })
   * ```
   */
  edge(callbacks: EdgeCallbacks<DataModel>) {
    const component = this.component
    const names = this.folderNames
    type QueryCtx = GenericQueryCtx<DataModel>
    type MutationCtx = GenericMutationCtx<DataModel>

    /** May the human behind a device password use this mailbox today? */
    const userAllowed = async (ctx: QueryCtx, mailbox: MailboxRef, userKey: string) => {
      if (userKey === mailbox.ownerKey) return true
      if (!callbacks.allowMember) return false
      return await callbacks.allowMember(ctx, { ...mailbox, userKey })
    }

    /**
     * Is the mailbox still there, and still owned by the owner its ledger belongs to? The component
     * has already checked that the device password belongs to that owner too.
     */
    const usable = async (ctx: QueryCtx, session: BridgeSession) => {
      const owner = await callbacks.mailboxOwner(ctx, session.mailboxKey)
      if (owner === null || owner !== session.ownerKey) return false
      return await userAllowed(ctx, session, session.userKey)
    }

    /**
     * The wrapper of every route with a session token: `null` if the token is unknown or expired,
     * its device password is revoked, the mailbox is gone or belongs to someone else today, or the
     * human behind the device password may no longer use it. Reads only.
     */
    const sessionOf = async (
      ctx: QueryCtx,
      tokenHash: string,
      now: number,
    ): Promise<BridgeSession | null> => {
      const found = await ctx.runQuery(component.sessions.check, { tokenHash, now })
      if (!found) return null
      const session = {
        mailboxKey: found.mailboxKey,
        ownerKey: found.ownerKey,
        userKey: found.userKey,
      }
      return (await usable(ctx, session)) ? session : null
    }

    const folderStates = async (ctx: QueryCtx, mailboxKey: string) =>
      await ctx.runQuery(component.folders.list, { mailboxKey })

    /** The folder for a name from a request, if this mailbox has it. */
    const folderOf = async (ctx: QueryCtx, mailboxKey: string, name: string) => {
      const key = folderKey(name, names)
      if (!key) return null
      const states = await folderStates(ctx, mailboxKey)
      const state = states.find((row) => row.folder === key)
      if (!state) return null
      return { key, state, extra: states.some((row) => isExtraFolder(row.folder)) }
    }

    return {
      /**
       * Sign-in with address and device password. Unknown address, wrong or revoked password, a
       * mailbox that is gone or transferred, a member who may no longer use it: all `auth`, with
       * the same work. The route builds the hashes and throttle keys; no secret arrives in clear.
       */
      login: internalMutationGeneric({
        args: loginArgs,
        returns: loginResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as MutationCtx
          const found = await callbacks.mailboxByAddress(ctx, args.address)
          // Looked up even without a mailbox, so known and unknown addresses cost the same.
          const holder = await ctx.runQuery(component.devices.holder, {
            mailboxKey: found?.mailboxKey ?? '',
            secretHash: args.secretHash,
          })
          const allowed =
            found !== null &&
            (holder === null || (await userAllowed(ctx, found, holder.userKey ?? found.ownerKey)))
          const mailbox = allowed ? found : null
          // A mailbox with more than INBOX and Sent only takes a bridge that knows folders: an
          // older one would send "delete" as a plain expunge. Password and throttle are checked
          // all the same, only no session is issued. The folders are read for every sign-in of an
          // older bridge, with an empty key when there is no mailbox, so that known and unknown
          // addresses make the same calls; whether that read happens depends on the bridge alone.
          const older = (args.bridge ?? 1) < BRIDGE_VERSION_FOLDERS
          const known = older ? await folderStates(ctx, mailbox?.mailboxKey ?? '') : []
          const tooOld = known.some((row) => isExtraFolder(row.folder))
          const result = await ctx.runMutation(component.sessions.login, {
            mailboxKey: mailbox?.mailboxKey ?? null,
            ownerKey: mailbox?.ownerKey ?? null,
            secretHash: args.secretHash,
            tokenHash: args.tokenHash,
            pairKey: args.pairKey,
            ipKey: args.ipKey,
            ...(tooOld ? { issue: false } : {}),
          })
          if (result.kind === 'ok' && mailbox) {
            if (tooOld) return { kind: 'preparing' as const }
            return { kind: 'ok' as const, expiresAt: result.expiresAt, mailbox: mailbox.mailboxKey }
          }
          // `ok` without a mailbox does not happen (the component counts a failed attempt then).
          return result.kind === 'ok' ? { kind: 'auth' as const } : result
        },
      }),

      /**
       * Change counters for many session tokens, in the order of the request. Each entry is
       * checked like in the wrapper; an invalid one gets `ok: false` and the bridge ends its
       * connection.
       */
      seq: internalQueryGeneric({
        args: seqArgs,
        returns: seqResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const sessions = await ctx.runQuery(component.sessions.seqMany, args)
          const known = new Map<string, boolean>()
          const results: Infer<typeof seqResult> = []
          for (const session of sessions) {
            if (!session) {
              results.push({ ok: false })
              continue
            }
            const key = `${session.mailboxKey}\n${session.ownerKey}\n${session.userKey}`
            let ok = known.get(key)
            if (ok === undefined) {
              ok = await usable(ctx, session)
              known.set(key, ok)
            }
            results.push(ok ? { ok: true, seq: session.seq } : { ok: false })
          }
          return results
        },
      }),

      /** The folders of the mailbox with UID and counter state. */
      folders: internalQueryGeneric({
        args: foldersArgs,
        returns: foldersResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const session = await sessionOf(ctx, args.tokenHash, args.now)
          if (!session) return NO_SESSION
          const rows = await folderStates(ctx, session.mailboxKey)
          return {
            kind: 'ok' as const,
            folders: rows.map((row) => ({
              ...names[row.folder],
              uidValidity: row.uidValidity,
              uidNext: row.uidNext,
              highestModseq: row.highestModseq,
              exists: row.exists,
              unseen: row.unseen,
            })),
          }
        },
      }),

      /** Live rows of a folder by UID, with the folder state from the same transaction. */
      entries: internalQueryGeneric({
        args: entriesArgs,
        returns: entriesResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const session = await sessionOf(ctx, args.tokenHash, args.now)
          if (!session) return NO_SESSION
          const folder = await folderOf(ctx, session.mailboxKey, args.folder)
          if (!folder) return NO_FOLDER
          // The component bounds every page itself; read on up to `limit` in this transaction.
          const rows: LedgerRow[] = []
          let afterUid = args.afterUid
          let isDone = false
          for (;;) {
            const page = await ctx.runQuery(component.entries.list, {
              mailboxKey: session.mailboxKey,
              folder: folder.key,
              limit: args.limit - rows.length,
              ...(afterUid !== undefined ? { afterUid } : {}),
            })
            for (const row of page.rows) rows.push(publicRow(row))
            isDone = page.isDone
            if (page.isDone || page.rows.length === 0 || rows.length >= args.limit) break
            afterUid = page.rows[page.rows.length - 1].uid
          }
          return {
            kind: 'ok' as const,
            rows,
            more: !isDone,
            uidValidity: folder.state.uidValidity,
            uidNext: folder.state.uidNext,
            highestModseq: folder.state.highestModseq,
          }
        },
      }),

      /**
       * Changes of a folder since a state, removed rows included (`live: false`). `resync`: the
       * state is older than what cleanup already deleted, or newer than anything the folder ever
       * had; the bridge drops its mirror.
       */
      changes: internalQueryGeneric({
        args: changesArgs,
        returns: changesResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const session = await sessionOf(ctx, args.tokenHash, args.now)
          if (!session) return NO_SESSION
          const folder = await folderOf(ctx, session.mailboxKey, args.folder)
          if (!folder) return NO_FOLDER
          const rows: Array<LedgerRow & { live: boolean }> = []
          let sinceModseq = args.sinceModseq
          let isDone = false
          for (;;) {
            const page = await ctx.runQuery(component.entries.changes, {
              mailboxKey: session.mailboxKey,
              folder: folder.key,
              sinceModseq,
              limit: args.limit - rows.length,
            })
            // What counts is the state of the caller, so the first page decides.
            if (rows.length === 0 && page.resync) return { kind: 'resync' as const }
            for (const row of page.rows) rows.push({ ...publicRow(row), live: row.live })
            isDone = page.isDone
            if (page.isDone || page.rows.length === 0 || rows.length >= args.limit) break
            sinceModseq = page.rows[page.rows.length - 1].modseq
          }
          return {
            kind: 'ok' as const,
            rows,
            more: !isDone,
            uidValidity: folder.state.uidValidity,
            uidNext: folder.state.uidNext,
            highestModseq: folder.state.highestModseq,
          }
        },
      }),

      /**
       * Where the raw message behind a UID is. `uidvalidity`: the bridge learned the UID under
       * another UIDVALIDITY and drops its mirror. `gone`: no live row with this UID in this
       * mailbox and folder, or your app has no bytes for it.
       */
      raw: internalQueryGeneric({
        args: rawArgs,
        returns: rawResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const session = await sessionOf(ctx, args.tokenHash, args.now)
          if (!session) return NO_SESSION
          const folder = await folderOf(ctx, session.mailboxKey, args.folder)
          if (!folder) return { kind: 'gone' as const }
          if (folder.state.uidValidity !== args.uidValidity) return { kind: 'uidvalidity' as const }
          const row = await ctx.runQuery(component.entries.byUid, {
            mailboxKey: session.mailboxKey,
            folder: folder.key,
            uid: args.uid,
          })
          if (!row) return { kind: 'gone' as const }
          const location = await callbacks.rawLocation(ctx, {
            ...session,
            messageKey: row.messageKey,
            folder: folder.key,
          })
          if (location === null) {
            console.warn(`[mail-bridge] row without raw message ${session.mailboxKey} ${folder.key} ${args.uid}`)
            return { kind: 'gone' as const }
          }
          return { kind: 'ok' as const, location, size: row.size }
        },
      }),

      /**
       * Sets and removes flags, in ONE mutation: the rows in the component and, where the request
       * names `\Seen`, your `onSeen`. The answer lists only rows that actually changed.
       */
      store: internalMutationGeneric({
        args: storeArgs,
        returns: storeResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as MutationCtx
          const session = await sessionOf(ctx, args.tokenHash, Date.now())
          if (!session) return NO_SESSION
          const folder = await folderOf(ctx, session.mailboxKey, args.folder)
          if (!folder) return NO_FOLDER
          if (args.uids.length > BRIDGE_LIMITS.uids) throw new Error('mailBridge: too many uids')
          const rows = await ctx.runMutation(component.entries.setFlags, {
            mailboxKey: session.mailboxKey,
            folder: folder.key,
            uids: args.uids,
            add: args.add,
            remove: args.remove,
          })
          // If the mail client named `\Seen`, its state counts: your data follows even where the
          // row already stood like that (row and message may have drifted apart).
          if (callbacks.onSeen && rows.length > 0 && namesSeenFlag([...args.add, ...args.remove])) {
            await callbacks.onSeen(ctx, {
              ...session,
              folder: folder.key,
              messages: rows.map((row) => ({
                messageKey: row.messageKey,
                seen: row.flags.includes(SEEN),
              })),
            })
          }
          return {
            kind: 'ok' as const,
            rows: rows
              .filter((row) => row.changed)
              .map((row) => ({ uid: row.uid, flags: row.flags, modseq: row.modseq }))
              .sort((a, b) => a.uid - b.uid),
          }
        },
      }),

      /** Removed in the mail client: the rows are hidden in the ledger, your data stays. */
      expunge: internalMutationGeneric({
        args: expungeArgs,
        returns: expungeResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as MutationCtx
          const session = await sessionOf(ctx, args.tokenHash, Date.now())
          if (!session) return NO_SESSION
          const folder = await folderOf(ctx, session.mailboxKey, args.folder)
          if (!folder) return NO_FOLDER
          // In a mailbox with folders, deleting outside Sent and Trash means "move to Trash"
          // (contract revision 1.2). Until `/move` exists nothing is hidden there, so that no
          // tombstone appears that the later rule does not know.
          if (folder.extra && folder.key !== 'Sent' && folder.key !== 'Trash') {
            return { kind: 'ok' as const, uids: [] }
          }
          const hidden = await ctx.runMutation(component.entries.hide, {
            mailboxKey: session.mailboxKey,
            folder: folder.key,
            uids: args.uids,
          })
          if (callbacks.onExpunge && hidden.length > 0) {
            await callbacks.onExpunge(ctx, {
              ...session,
              folder: folder.key,
              messageKeys: hidden.map((row) => row.messageKey),
            })
          }
          return { kind: 'ok' as const, uids: hidden.map((row) => row.uid).sort((a, b) => a - b) }
        },
      }),

      /**
       * The mail client wants to store its copy of a sent message in `Sent`. If your app knows a
       * sent message with this Message-ID that has a row there, the client gets its UID; otherwise
       * `unknown`. Nothing is ever stored.
       */
      append: internalQueryGeneric({
        args: appendArgs,
        returns: appendResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          const session = await sessionOf(ctx, args.tokenHash, args.now)
          if (!session) return NO_SESSION
          if (folderKey(args.folder, names) !== 'Sent') return NO_FOLDER
          const states = await folderStates(ctx, session.mailboxKey)
          const sent = states.find((row) => row.folder === 'Sent')
          const keys = callbacks.findSent
            ? await callbacks.findSent(ctx, { ...session, messageId: args.messageId })
            : []
          if (sent) {
            for (const messageKey of keys.slice(0, APPEND_CANDIDATES)) {
              const entry = await ctx.runQuery(component.entries.byMessage, { messageKey })
              // The row is held against the mailbox of the session once more.
              if (!entry || entry.mailboxKey !== session.mailboxKey) continue
              if (entry.folder === 'Sent') {
                return { kind: 'ok' as const, uid: entry.uid, uidValidity: sent.uidValidity }
              }
              // Stored, but the sent message lies in Trash: no UID.
              if (entry.folder === 'Trash') return { kind: 'ok' as const }
            }
          }
          return { kind: 'unknown' as const }
        },
      }),

      /**
       * First step of a submission, before the route reads the body (up to 5 MiB): is the session
       * valid, and does your `allowSubmit` let it through? This check is early, not final: reading
       * the body and sending take time. The step that records the message asks `session` again.
       */
      submitCheck: internalMutationGeneric({
        args: submitCheckArgs,
        returns: submitCheckResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as MutationCtx
          const session = await sessionOf(ctx, args.tokenHash, Date.now())
          if (!session) return NO_SESSION
          if (callbacks.allowSubmit && !(await callbacks.allowSubmit(ctx, session))) {
            return { kind: 'limit' as const }
          }
          return { kind: 'ok' as const, ...session }
        },
      }),

      /**
       * The session behind `Submission.tokenHash`, or `null` if it has ended since the route
       * checked it. The same check as the wrapper of every route; it reads only and counts
       * nothing (your `allowSubmit` is not asked again).
       *
       * Call it in the mutation of your app that records a submitted message, so that check and
       * write are one transaction, and take the mailbox from its result:
       *
       * ```ts
       * const session = await ctx.runQuery(internal.mailBridge.session, { tokenHash, now: Date.now() })
       * if (!session || session.mailboxKey !== mailboxId) return null // answer { status: 'session' }
       * ```
       */
      session: internalQueryGeneric({
        args: sessionCheckArgs,
        returns: sessionCheckResult,
        handler: async (rawCtx, args) => {
          const ctx = rawCtx as unknown as QueryCtx
          return await sessionOf(ctx, args.tokenHash, args.now)
        },
      }),

      /** Housekeeping, see `MailBridge.cleanup`. Schedule it hourly from your `crons.ts`. */
      cleanup: internalMutationGeneric({
        args: {},
        returns: cleanupResult,
        handler: async (ctx) => await this.cleanup(ctx),
      }),
    }
  }

  // ─── Routes ───────────────────────────────────────────────────────────

  /**
   * Mounts the routes of the edge contract (`POST /bridge/v1/<route>`) on the HTTP router of your
   * app. One wrapper for all of them: first the shared secret, before the body is read; then,
   * except for `/login` and `/seq`, the shape of the session token from `X-Bridge-Session`.
   * Whether the token is valid is decided by the function behind the route, in the transaction of
   * its work; for `/submit` that work is your `submit`, see `Submission.tokenHash`. Logs name only
   * the route and the kind of error.
   */
  registerRoutes(http: HttpRouter, options: RouteOptions<DataModel>) {
    const fn = options.functions
    const storeBatch = this.options.storeBatch ?? 10
    const component = this.component
    type Ctx = GenericActionCtx<DataModel>
    type Input = { request: Request; tokenHash: string; now: number; secret: string }
    type Route = { session: boolean; run: (ctx: Ctx, input: Input) => Promise<Response> }

    const problem = (found: RequestProblem) => bridgeError(found.status, found.code)
    const sessionExpired = () => bridgeError(401, 'session')
    const unknownFolder = () => bridgeError(404, 'folder')
    const loadRaw =
      options.loadRaw ??
      (async (ctx: Ctx, location: string) => {
        try {
          return await ctx.storage.get(location as GenericId<'_storage'>)
        } catch (error) {
          // `rawLocation` returned something that is not an id of Convex file storage. For the mail
          // client that message has no bytes: `gone`, not a 500 on every attempt. The location
          // itself is not logged, it is your app's value.
          console.warn(
            `[mail-bridge] raw: location is not readable as a file in Convex storage (${
              error instanceof Error ? error.name : 'unknown'
            })`,
          )
          return null
        }
      })

    const routes: Record<string, Route> = {
      login: {
        session: false,
        run: async (ctx, { request, secret }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseLogin(body.value)
          if (!parsed.ok) return problem(parsed)
          // Hashed and counted always, also for addresses that do not exist. The edge creates the
          // token (256 bit); only its hash is stored.
          const session = newToken()
          const keys = await throttleKeys(secret, parsed.value.address, parsed.value.ip)
          const result = await ctx.runMutation(fn.login, {
            address: parsed.value.address,
            secretHash: await secretHash(parsed.value.password),
            tokenHash: await secretHash(session),
            bridge: parsed.value.bridge,
            ...keys,
          })
          if (result.kind === 'ok') {
            return bridgeJson({ session, expiresAt: result.expiresAt, mailbox: result.mailbox })
          }
          if (result.kind === 'preparing') return bridgeError(503, 'preparing')
          if (result.kind === 'throttled') {
            return bridgeError(
              429,
              'throttled',
              { retryAfterSeconds: result.retryAfterSeconds },
              { 'Retry-After': String(result.retryAfterSeconds) },
            )
          }
          return bridgeError(401, 'auth')
        },
      },

      seq: {
        session: false,
        run: async (ctx, { request, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseSeq(body.value)
          if (!parsed.ok) return problem(parsed)
          const tokenHashes: string[] = []
          for (const session of parsed.value.sessions) {
            tokenHashes.push(session ? await secretHash(session) : '')
          }
          const results = await ctx.runQuery(fn.seq, { tokenHashes, now })
          return bridgeJson({ results })
        },
      },

      folders: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseEmpty(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runQuery(fn.folders, { tokenHash, now })
          if (result.kind === 'session') return sessionExpired()
          return bridgeJson({ folders: result.folders })
        },
      },

      entries: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseEntries(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runQuery(fn.entries, { tokenHash, now, ...parsed.value })
          if (result.kind === 'session') return sessionExpired()
          if (result.kind === 'folder') return unknownFolder()
          const { kind: _kind, ...page } = result
          return bridgeJson(page)
        },
      },

      changes: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseChanges(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runQuery(fn.changes, { tokenHash, now, ...parsed.value })
          if (result.kind === 'session') return sessionExpired()
          if (result.kind === 'folder') return unknownFolder()
          if (result.kind === 'resync') return bridgeError(410, 'resync')
          const { kind: _kind, ...page } = result
          return bridgeJson(page)
        },
      },

      raw: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseRaw(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runQuery(fn.raw, { tokenHash, now, ...parsed.value })
          if (result.kind === 'session') return sessionExpired()
          if (result.kind === 'gone') return bridgeError(404, 'gone')
          if (result.kind === 'uidvalidity') return bridgeError(409, 'uidvalidity')
          // The bytes themselves, never a storage URL: that would be valid without signing in.
          const bytes = await loadRaw(ctx, result.location)
          if (!bytes) return bridgeError(404, 'gone')
          const length = bytes instanceof Blob ? bytes.size : bytes.byteLength
          if (length !== result.size) {
            // Mail clients compare the size they were told with what they get.
            console.warn(`[mail-bridge] raw: size differs from the ledger (${length} != ${result.size})`)
          }
          return new Response(bytes as Blob | ArrayBuffer | RawBytes, {
            status: 200,
            headers: {
              'Content-Type': 'message/rfc822',
              'Content-Length': String(length),
              'Cache-Control': 'no-store',
            },
          })
        },
      },

      store: {
        session: true,
        run: async (ctx, { request, tokenHash }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseStore(body.value)
          if (!parsed.ok) return problem(parsed)
          // In small batches: each is its own mutation (session, flags and `onSeen` together) and
          // stays below the read budget of a transaction. If the session ends in between, the
          // earlier batches stay written and the bridge ends the connection.
          const { folder, add, remove } = parsed.value
          const rows: Array<{ uid: number; flags: string[]; modseq: number }> = []
          for (const uids of inBatches([...new Set(parsed.value.uids)], storeBatch)) {
            const result = await ctx.runMutation(fn.store, { tokenHash, folder, uids, add, remove })
            if (result.kind === 'session') return sessionExpired()
            if (result.kind === 'folder') return unknownFolder()
            rows.push(...result.rows)
          }
          return bridgeJson({ rows: rows.sort((a, b) => a.uid - b.uid) })
        },
      },

      expunge: {
        session: true,
        run: async (ctx, { request, tokenHash }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseExpunge(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runMutation(fn.expunge, { tokenHash, ...parsed.value })
          if (result.kind === 'session') return sessionExpired()
          if (result.kind === 'folder') return unknownFolder()
          return bridgeJson({ uids: result.uids })
        },
      },

      append: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          const body = await readJsonObject(request)
          if (!body.ok) return problem(body)
          const parsed = parseAppend(body.value)
          if (!parsed.ok) return problem(parsed)
          const result = await ctx.runQuery(fn.append, { tokenHash, now, ...parsed.value })
          if (result.kind === 'session') return sessionExpired()
          if (result.kind === 'folder') return bridgeError(403, 'folder')
          if (result.kind === 'unknown') return bridgeError(409, 'unknown')
          if (result.uid === undefined) return bridgeJson({})
          return bridgeJson({ uid: result.uid, uidValidity: result.uidValidity })
        },
      },

      submit: {
        session: true,
        run: async (ctx, { request, tokenHash, now }) => {
          // Without a way to send there is nothing to count: the session is only read, your
          // `allowSubmit` is not asked.
          if (!options.submit) {
            const open = await ctx.runQuery(fn.session, { tokenHash, now })
            return open ? bridgeSubmitRejection('unavailable') : sessionExpired()
          }
          // First the session and your `allowSubmit`, then the body: for an invalid session or a
          // mailbox over its limit no raw message is read.
          const session = await ctx.runMutation(fn.submitCheck, { tokenHash })
          if (session.kind === 'session') return sessionExpired()
          if (session.kind === 'limit') return bridgeSubmitRejection('limit')
          const envelope = parseEnvelope(
            request.headers.get('x-envelope-from'),
            request.headers.get('x-envelope-to'),
          )
          if (!envelope.ok) return problem(envelope)
          const raw = await readLimitedBody(request, BRIDGE_LIMITS.submitBytes)
          if (raw === null) return bridgeSubmitRejection('size')
          if (raw.byteLength === 0) return bridgeSubmitRejection('content')
          // The check above is some time ago by now (the body, soon the provider). Whether the
          // session still holds is decided where the message is recorded: your `submit` passes
          // `tokenHash` to the mutation that does it, and that mutation asks `session`.
          const result = await options.submit(ctx, {
            mailboxKey: session.mailboxKey,
            ownerKey: session.ownerKey,
            userKey: session.userKey,
            tokenHash,
            envelope: envelope.value,
            raw,
            digest: await sha256Hex(raw),
          })
          if (result.status === 'session') return sessionExpired()
          if (result.status === 'rejected') return bridgeSubmitRejection(result.reason, result.message)
          // Both are a `250` for the mail client.
          if (result.status === 'unknown') return bridgeJson({ status: 'unknown' })
          let uid: number | undefined
          if (result.messageKey !== undefined) {
            const entry = await ctx.runQuery(component.entries.byMessage, {
              messageKey: result.messageKey,
            })
            if (entry?.live && entry.folder === 'Sent' && entry.mailboxKey === session.mailboxKey) {
              uid = entry.uid
            }
          }
          return bridgeJson({ status: 'sent', ...(uid !== undefined ? { uid } : {}) })
        },
      },
    }

    const serve = async (ctx: Ctx, request: Request, name: string, route: Route) => {
      const secret = this.secret()
      if (!secret || !(await bridgeAuthorized(request.headers.get('authorization'), secret))) {
        return bridgeUnauthorized()
      }
      try {
        let tokenHash = ''
        if (route.session) {
          const token = request.headers.get('x-bridge-session') ?? ''
          if (!isTokenShape(token)) return sessionExpired()
          tokenHash = await secretHash(token)
        }
        return await route.run(ctx, { request, tokenHash, now: Date.now(), secret })
      } catch (error) {
        console.error(
          `[mail-bridge] ${name} failed: ${error instanceof Error ? error.name : 'unknown'}`,
        )
        return bridgeError(500, 'internal')
      }
    }

    for (const [name, route] of Object.entries(routes)) {
      http.route({
        path: `${BRIDGE_BASE}/${name}`,
        method: 'POST',
        handler: httpActionGeneric(
          async (ctx, request) => await serve(ctx as unknown as Ctx, request, name, route),
        ),
      })
    }
  }
}

type LedgerRow = { uid: number; flags: string[]; modseq: number; size: number; internalDate: number }

function publicRow(row: LedgerRow): LedgerRow {
  return {
    uid: row.uid,
    flags: row.flags,
    modseq: row.modseq,
    size: row.size,
    internalDate: row.internalDate,
  }
}
