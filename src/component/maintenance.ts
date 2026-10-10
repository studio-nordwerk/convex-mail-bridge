// Maintenance: remove tombstones of permanently deleted messages after the retention period, and a
// paginated read function for the app's reconcile (rows <-> messages).

import { v } from 'convex/values'
import { mutation, query } from './_generated/server.js'
import type { Doc } from './_generated/dataModel.js'
import { folderValidator, removedByValidator } from './schema.js'
import { findFolder, pageSize, type Folder } from './lib/ledger.js'

/**
 * Deletes tombstones created before `before`: rows of permanently deleted messages (`purge`)
 * and traces of earlier folders (`move`). Tombstones from the mail client (`client`) stay while
 * the message is hidden there: only `entries.remove` turns them into `purge`, only a folder change
 * into `move`. Nothing changes for mail clients (the row was already removed), so the change
 * state and `seq` stay untouched.
 *
 * Per folder it records up to which change state removed rows have been deleted
 * (`resyncBelowModseq`, the maximum of the deleted ones). A mirror with an older state can no
 * longer learn about this removal; `entries.changes` reports `resync` to it.
 */
export const cleanup = mutation({
  args: { before: v.number(), limit: v.number() },
  returns: v.object({ deleted: v.number(), isDone: v.boolean() }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const rows: Array<Doc<'entries'>> = []
    for (const removedBy of ['purge', 'move'] as const) {
      const page = await ctx.db
        .query('entries')
        .withIndex('by_removedBy_and_removedAt', (q) =>
          q.eq('removedBy', removedBy).lt('removedAt', args.before),
        )
        .take(limit + 1 - rows.length)
      rows.push(...page)
      if (rows.length > limit) break
    }
    const batch = rows.slice(0, limit)
    const pruned = new Map<string, { mailboxKey: string; folder: Folder; modseq: number }>()
    for (const row of batch) {
      const key = `${row.folder}\n${row.mailboxKey}`
      const modseq = row.removedModseq ?? row.modseq
      const known = pruned.get(key)
      if (!known || modseq > known.modseq) {
        pruned.set(key, { mailboxKey: row.mailboxKey, folder: row.folder, modseq })
      }
      await ctx.db.delete('entries', row._id)
    }
    for (const mark of pruned.values()) {
      const folder = await findFolder(ctx, mark.mailboxKey, mark.folder)
      if (folder && mark.modseq > (folder.resyncBelowModseq ?? 0)) {
        await ctx.db.patch('folders', folder._id, { resyncBelowModseq: mark.modseq })
      }
    }
    return { deleted: batch.length, isDone: rows.length <= limit }
  },
})

/**
 * Rows of a folder for the reconcile, by UID: either the live ones or the tombstones.
 * Paginate with `afterUid` (UID of the last row).
 */
export const scan = query({
  args: {
    mailboxKey: v.string(),
    folder: folderValidator,
    live: v.boolean(),
    afterUid: v.optional(v.number()),
    limit: v.number(),
  },
  returns: v.object({
    rows: v.array(
      v.object({
        uid: v.number(),
        messageKey: v.string(),
        flags: v.array(v.string()),
        removedBy: v.optional(removedByValidator),
        /** Does the row carry group and kind? Without both, `entries.moveGroup` does not find it. */
        tagged: v.boolean(),
      }),
    ),
    isDone: v.boolean(),
  }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const rows = await ctx.db
      .query('entries')
      .withIndex('by_mailboxKey_and_folder_and_live_and_uid', (q) =>
        q
          .eq('mailboxKey', args.mailboxKey)
          .eq('folder', args.folder)
          .eq('live', args.live)
          .gt('uid', args.afterUid ?? 0),
      )
      .take(limit + 1)
    return {
      rows: rows.slice(0, limit).map((row) => ({
        uid: row.uid,
        messageKey: row.messageKey,
        flags: row.flags,
        ...(row.removedBy !== undefined ? { removedBy: row.removedBy } : {}),
        tagged: row.groupKey !== undefined && row.kind !== undefined,
      })),
      isDone: rows.length <= limit,
    }
  },
})
