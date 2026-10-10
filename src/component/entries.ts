// UID ledger rows: exactly one per message, with a fixed UID in the folder.
//
// Writing always goes through the bookkeeping (`lib/ledger.ts`): own change stamp per changed row,
// folder counters, change counter of the mailbox. A call that changes nothing writes nothing.

import { v } from 'convex/values'
import { mutation, query } from './_generated/server.js'
import type { Doc } from './_generated/dataModel.js'
import { folderValidator, kindValidator, removedByValidator } from './schema.js'
import { applyFlagChange, hasSeen, normalizeFlags, sameFlags, withSeen } from './lib/flags.js'
import { findEntry, findFolder, findLiveEntry, findState, openLedger, pageSize } from './lib/ledger.js'
import { placeEntry, tagDiffers, tagFields } from './lib/place.js'

const rowFields = {
  uid: v.number(),
  messageKey: v.string(),
  modseq: v.number(),
  size: v.number(),
  internalDate: v.number(),
  flags: v.array(v.string()),
}

function rowOf(entry: Doc<'entries'>) {
  return {
    uid: entry.uid,
    messageKey: entry.messageKey,
    modseq: entry.modseq,
    size: entry.size,
    internalDate: entry.internalDate,
    flags: entry.flags,
  }
}

function removalOf(entry: Doc<'entries'>) {
  return {
    ...(entry.removedAt !== undefined ? { removedAt: entry.removedAt } : {}),
    ...(entry.removedModseq !== undefined ? { removedModseq: entry.removedModseq } : {}),
    ...(entry.removedBy !== undefined ? { removedBy: entry.removedBy } : {}),
  }
}

function requireCount(value: number, name: string) {
  if (!Number.isFinite(value) || value < 0) throw new Error(`mailBridge: invalid ${name}`)
}

/**
 * UID for a message in the folder `folder`. `null` if the mailbox is not enabled. Repeatable: if
 * the message already lies there (live or hidden in the mail client) or has been permanently
 * deleted, the call changes nothing and returns the existing UID. If it lies in another folder, it
 * moves here and gets a new UID (`moved`; rule: `lib/place.ts`).
 * `groupKey` and `kind` are recorded on the row, also afterwards.
 */
export const add = mutation({
  args: {
    mailboxKey: v.string(),
    folder: folderValidator,
    messageKey: v.string(),
    size: v.number(),
    internalDate: v.number(),
    seen: v.boolean(),
    flags: v.optional(v.array(v.string())),
    groupKey: v.optional(v.string()),
    kind: v.optional(kindValidator),
  },
  returns: v.union(
    v.null(),
    v.object({ uid: v.number(), created: v.boolean(), moved: v.boolean() }),
  ),
  handler: async (ctx, args) => {
    if (!(await findState(ctx, args.mailboxKey))) return null
    const current = await findEntry(ctx, args.messageKey)
    const tag = { groupKey: args.groupKey, kind: args.kind }
    if (!current) {
      requireCount(args.size, 'size')
      requireCount(args.internalDate, 'internalDate')
    }
    const ledger = openLedger(ctx, args.mailboxKey)
    const placed = await placeEntry(ctx, ledger, {
      mailboxKey: args.mailboxKey,
      messageKey: args.messageKey,
      target: args.folder,
      current,
      fresh: {
        size: args.size,
        internalDate: args.internalDate,
        seen: args.seen,
        flags: args.flags ?? [],
      },
      tag,
      now: Date.now(),
    })
    await ledger.commit()
    if (placed.change !== 'none') {
      return {
        uid: placed.uid,
        created: placed.change === 'created',
        moved: placed.change === 'moved',
      }
    }
    // Nothing to do; only add group and kind (no mail client sees that).
    const entry = placed.entry
    if (!entry) return null
    if (entry.mailboxKey === args.mailboxKey && tagDiffers(entry, tag)) {
      await ctx.db.patch('entries', entry._id, tagFields(tag))
    }
    return { uid: entry.uid, created: false, moved: false }
  },
})

/**
 * Record group and kind on the current rows (for rows from the time before). No mail client sees
 * that: neither `modseq` nor `seq` changes. Returns the number of changed rows.
 */
export const tagMany = mutation({
  args: {
    mailboxKey: v.string(),
    rows: v.array(v.object({ messageKey: v.string(), groupKey: v.string(), kind: kindValidator })),
  },
  returns: v.number(),
  handler: async (ctx, args) => {
    let changed = 0
    for (const row of args.rows) {
      const entry = await findEntry(ctx, row.messageKey)
      if (!entry || entry.mailboxKey !== args.mailboxKey) continue
      const tag = { groupKey: row.groupKey, kind: row.kind }
      if (!tagDiffers(entry, tag)) continue
      await ctx.db.patch('entries', entry._id, tagFields(tag))
      changed += 1
    }
    return changed
  },
})

/**
 * Bring all messages of a group into their target folders: one folder per kind (`in`, `out`); if a
 * kind is missing, its rows stay where they are. The rule from `lib/place.ts` applies, also to
 * messages hidden in the mail client. At most `limit` moves per call; `isDone: false` means: call
 * again. A call without effect writes nothing.
 */
export const moveGroup = mutation({
  args: {
    mailboxKey: v.string(),
    groupKey: v.string(),
    targets: v.object({ in: v.optional(folderValidator), out: v.optional(folderValidator) }),
    limit: v.number(),
  },
  returns: v.object({
    moved: v.array(
      v.object({
        messageKey: v.string(),
        from: folderValidator,
        fromUid: v.number(),
        /** `false`: the message was hidden in the mail client there. */
        fromLive: v.boolean(),
        to: folderValidator,
        uid: v.number(),
      }),
    ),
    isDone: v.boolean(),
  }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    // Read first, then write. Only rows not lying in the target folder are read (per kind and
    // state the folders before and after it): if there is nothing to do, nothing is read, and a
    // concurrent entry in the target folder does not disturb the call.
    const due: Array<{ entry: Doc<'entries'>; target: Doc<'entries'>['folder'] }> = []
    scan: for (const kind of ['in', 'out'] as const) {
      const target = args.targets[kind]
      if (!target) continue
      for (const removedBy of [undefined, 'client'] as const) {
        for (const side of ['before', 'after'] as const) {
          const rows = await ctx.db
            .query('entries')
            .withIndex('by_mailboxKey_and_groupKey_and_kind_and_removedBy_and_folder', (q) => {
              const group = q
                .eq('mailboxKey', args.mailboxKey)
                .eq('groupKey', args.groupKey)
                .eq('kind', kind)
                .eq('removedBy', removedBy)
              return side === 'before' ? group.lt('folder', target) : group.gt('folder', target)
            })
            .take(limit + 1 - due.length)
          for (const entry of rows) due.push({ entry, target })
          if (due.length > limit) break scan
        }
      }
    }
    const isDone = due.length <= limit
    due.length = Math.min(due.length, limit)
    const ledger = openLedger(ctx, args.mailboxKey)
    const now = Date.now()
    const moved = []
    for (const { entry, target } of due) {
      const placed = await placeEntry(ctx, ledger, {
        mailboxKey: args.mailboxKey,
        messageKey: entry.messageKey,
        target,
        current: entry,
        now,
      })
      if (placed.change !== 'moved') continue
      moved.push({
        messageKey: entry.messageKey,
        from: placed.from.folder,
        fromUid: placed.from.uid,
        fromLive: placed.from.live,
        to: target,
        uid: placed.uid,
      })
    }
    await ledger.commit()
    return { moved, isDone }
  },
})

/**
 * Read state of several messages of a mailbox. Returns how many live rows changed. Unknown,
 * permanently deleted and foreign messages are skipped; on a row hidden in the mail client the
 * state is recorded silently (no change stamp).
 */
export const setSeenMany = mutation({
  args: {
    mailboxKey: v.string(),
    changes: v.array(v.object({ messageKey: v.string(), seen: v.boolean() })),
  },
  returns: v.number(),
  handler: async (ctx, args) => {
    const ledger = openLedger(ctx, args.mailboxKey)
    let changed = 0
    for (const change of args.changes) {
      const entry = await findEntry(ctx, change.messageKey)
      if (!entry || entry.mailboxKey !== args.mailboxKey || entry.removedBy === 'purge') continue
      const seen = hasSeen(entry.flags)
      if (seen === change.seen) continue
      if (!entry.live) {
        // Hidden in the mail client: nobody sees the row, but it carries the read state on, with
        // which the message reappears in another folder.
        await ctx.db.patch('entries', entry._id, { flags: withSeen(entry.flags, change.seen) })
        continue
      }
      const modseq = await ledger.change(entry.folder, { seen }, { seen: change.seen })
      await ctx.db.patch('entries', entry._id, {
        flags: withSeen(entry.flags, change.seen),
        modseq,
      })
      changed += 1
    }
    await ledger.commit()
    return changed
  },
})

/**
 * Set and remove flags from the mail client. For each live row found, the state afterwards
 * (`modseq` also if nothing changed); `changed` names those actually changed, `seenChanged` tells
 * the app that it has to carry the message's read state forward.
 */
export const setFlags = mutation({
  args: {
    mailboxKey: v.string(),
    folder: folderValidator,
    uids: v.array(v.number()),
    add: v.array(v.string()),
    remove: v.array(v.string()),
  },
  returns: v.array(
    v.object({
      uid: v.number(),
      messageKey: v.string(),
      flags: v.array(v.string()),
      modseq: v.number(),
      changed: v.boolean(),
      seenChanged: v.boolean(),
    }),
  ),
  handler: async (ctx, args) => {
    const ledger = openLedger(ctx, args.mailboxKey)
    const result = []
    for (const uid of new Set(args.uids)) {
      const entry = await findLiveEntry(ctx, args.mailboxKey, args.folder, uid)
      if (!entry) continue
      const current = normalizeFlags(entry.flags)
      const next = applyFlagChange(current, args.add, args.remove)
      const seenBefore = hasSeen(current)
      const seenAfter = hasSeen(next)
      const changed = !sameFlags(entry.flags, next)
      let modseq = entry.modseq
      if (changed) {
        modseq = await ledger.change(args.folder, { seen: seenBefore }, { seen: seenAfter })
        await ctx.db.patch('entries', entry._id, { flags: next, modseq })
      }
      result.push({
        uid: entry.uid,
        messageKey: entry.messageKey,
        flags: next,
        modseq,
        changed,
        seenChanged: seenBefore !== seenAfter,
      })
    }
    await ledger.commit()
    return result
  },
})

/**
 * The message was permanently deleted in the app. Its current row stays as a tombstone (`purge`)
 * until `maintenance.cleanup` deletes it after the period; traces of earlier folders (`move`) are
 * cleared by the same run after their own period. If it was already removed in the mail client,
 * nothing changes for mail clients any more: only the kind of tombstone and the period.
 */
export const remove = mutation({
  args: { messageKey: v.string() },
  returns: v.boolean(),
  handler: async (ctx, args) => {
    const entry = await findEntry(ctx, args.messageKey)
    if (!entry) return false
    const now = Date.now()
    if (!entry.live) {
      if (entry.removedBy !== 'purge') {
        await ctx.db.patch('entries', entry._id, { removedBy: 'purge', removedAt: now })
      }
      return true
    }
    const ledger = openLedger(ctx, entry.mailboxKey)
    const modseq = await ledger.change(entry.folder, { seen: hasSeen(entry.flags) }, null)
    await ctx.db.patch('entries', entry._id, {
      live: false,
      modseq,
      removedAt: now,
      removedModseq: modseq,
      removedBy: 'purge',
    })
    await ledger.commit()
    return true
  },
})

/**
 * Removed in the mail client: the message still exists, the row stays as a tombstone (`client`).
 * Returns the rows that disappeared as a result.
 */
export const hide = mutation({
  args: { mailboxKey: v.string(), folder: folderValidator, uids: v.array(v.number()) },
  returns: v.array(v.object({ uid: v.number(), messageKey: v.string() })),
  handler: async (ctx, args) => {
    const ledger = openLedger(ctx, args.mailboxKey)
    const now = Date.now()
    const hidden = []
    for (const uid of new Set(args.uids)) {
      const entry = await findLiveEntry(ctx, args.mailboxKey, args.folder, uid)
      if (!entry) continue
      const modseq = await ledger.change(args.folder, { seen: hasSeen(entry.flags) }, null)
      await ctx.db.patch('entries', entry._id, {
        live: false,
        modseq,
        removedAt: now,
        removedModseq: modseq,
        removedBy: 'client',
      })
      hidden.push({ uid: entry.uid, messageKey: entry.messageKey })
    }
    await ledger.commit()
    return hidden
  },
})

/** Live rows of a folder by UID, paging with `afterUid` (UID of the last row). */
export const list = query({
  args: {
    mailboxKey: v.string(),
    folder: folderValidator,
    afterUid: v.optional(v.number()),
    limit: v.number(),
  },
  returns: v.object({ rows: v.array(v.object(rowFields)), isDone: v.boolean() }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const rows = await ctx.db
      .query('entries')
      .withIndex('by_mailboxKey_and_folder_and_live_and_uid', (q) =>
        q
          .eq('mailboxKey', args.mailboxKey)
          .eq('folder', args.folder)
          .eq('live', true)
          .gt('uid', args.afterUid ?? 0),
      )
      .take(limit + 1)
    return { rows: rows.slice(0, limit).map(rowOf), isDone: rows.length <= limit }
  },
})

/**
 * What changed in the folder since a state, by change stamp, including removed rows (`live:
 * false`). Page with the `modseq` of the last row; because every change has its own value,
 * nothing is lost at the page boundary.
 *
 * `resync: true`: the caller's state cannot be carried forward, it has to re-read the folder. Two
 * cases: `sinceModseq` is below the folder's `resyncBelowModseq` (cleanup deleted a removed row
 * whose removal this state never saw; the answer would be incomplete), or `sinceModseq` is above
 * `highestModseq` (a state that never existed in this folder, e.g. from another folder or from
 * before a renumbering).
 */
export const changes = query({
  args: {
    mailboxKey: v.string(),
    folder: folderValidator,
    sinceModseq: v.number(),
    limit: v.number(),
  },
  returns: v.object({
    rows: v.array(
      v.object({
        ...rowFields,
        live: v.boolean(),
        removedAt: v.optional(v.number()),
        removedModseq: v.optional(v.number()),
        removedBy: v.optional(removedByValidator),
      }),
    ),
    isDone: v.boolean(),
    resync: v.boolean(),
  }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const folder = await findFolder(ctx, args.mailboxKey, args.folder)
    const rows = await ctx.db
      .query('entries')
      .withIndex('by_mailboxKey_and_folder_and_modseq', (q) =>
        q
          .eq('mailboxKey', args.mailboxKey)
          .eq('folder', args.folder)
          .gt('modseq', args.sinceModseq),
      )
      .take(limit + 1)
    return {
      rows: rows
        .slice(0, limit)
        .map((entry) => ({ ...rowOf(entry), live: entry.live, ...removalOf(entry) })),
      isDone: rows.length <= limit,
      resync:
        args.sinceModseq < (folder?.resyncBelowModseq ?? 0) ||
        (folder !== null && args.sinceModseq > folder.highestModseq),
    }
  },
})

/**
 * The live row for a UID in exactly this mailbox and folder, or `null`. The path from a UID in a
 * request to the message: mailbox and folder are checked too.
 */
export const byUid = query({
  args: { mailboxKey: v.string(), folder: folderValidator, uid: v.number() },
  returns: v.union(v.null(), v.object(rowFields)),
  handler: async (ctx, args) => {
    const entry = await findLiveEntry(ctx, args.mailboxKey, args.folder, args.uid)
    return entry ? rowOf(entry) : null
  },
})

/**
 * From a UID that a mail client names to the message's live row: if the row is live, it is itself.
 * If it is a `move` tombstone (the message has meanwhile changed folder), the live row in the new
 * folder comes back. Otherwise `null`.
 */
export const resolve = query({
  args: { mailboxKey: v.string(), folder: folderValidator, uid: v.number() },
  returns: v.union(
    v.null(),
    v.object({ ...rowFields, folder: folderValidator, moved: v.boolean() }),
  ),
  handler: async (ctx, args) => {
    const live = await findLiveEntry(ctx, args.mailboxKey, args.folder, args.uid)
    if (live) return { ...rowOf(live), folder: live.folder, moved: false }
    const gone = await ctx.db
      .query('entries')
      .withIndex('by_mailboxKey_and_folder_and_live_and_uid', (q) =>
        q
          .eq('mailboxKey', args.mailboxKey)
          .eq('folder', args.folder)
          .eq('live', false)
          .eq('uid', args.uid),
      )
      .unique()
    if (!gone || gone.removedBy !== 'move') return null
    const current = await findEntry(ctx, gone.messageKey)
    if (!current || !current.live || current.mailboxKey !== args.mailboxKey) return null
    return { ...rowOf(current), folder: current.folder, moved: true }
  },
})

/** The current row of a message (live, otherwise `client` or `purge` tombstone), or `null`. */
export const byMessage = query({
  args: { messageKey: v.string() },
  returns: v.union(
    v.null(),
    v.object({
      ...rowFields,
      mailboxKey: v.string(),
      folder: folderValidator,
      live: v.boolean(),
      removedAt: v.optional(v.number()),
      removedModseq: v.optional(v.number()),
      removedBy: v.optional(removedByValidator),
      groupKey: v.optional(v.string()),
      kind: v.optional(kindValidator),
    }),
  ),
  handler: async (ctx, args) => {
    const entry = await findEntry(ctx, args.messageKey)
    if (!entry) return null
    return {
      ...rowOf(entry),
      mailboxKey: entry.mailboxKey,
      folder: entry.folder,
      live: entry.live,
      ...removalOf(entry),
      ...tagFields(entry),
    }
  },
})
