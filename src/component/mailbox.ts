// Mailbox in the UID ledger: enable, read state, advance the backfill state.

import { v } from 'convex/values'
import { mutation, query, type MutationCtx } from './_generated/server.js'
import { backfillStatusValidator, folderValidator } from './schema.js'
import { BASE_FOLDERS, findFolder, findState, pageSize, type Folder } from './lib/ledger.js'

const stateValidator = v.object({
  ownerKey: v.string(),
  enabledAt: v.number(),
  seq: v.number(),
  backfillStatus: backfillStatusValidator,
  backfillFolder: v.optional(folderValidator),
  backfillCursor: v.optional(v.string()),
  backfillRun: v.number(),
  backfillUpdatedAt: v.number(),
  repairCursor: v.optional(v.string()),
})

/**
 * Creates the state and the folders `INBOX` and `Sent`. Idempotent: a second call changes
 * nothing, not even the owner. The backfill counts as running until the app reports it done.
 */
export const enable = mutation({
  args: { mailboxKey: v.string(), ownerKey: v.string() },
  returns: v.object({ created: v.boolean(), enabledAt: v.number() }),
  handler: async (ctx, args) => {
    const existing = await findState(ctx, args.mailboxKey)
    if (existing) return { created: false, enabledAt: existing.enabledAt }
    const now = Date.now()
    for (const folder of BASE_FOLDERS) await ensureFolder(ctx, args.mailboxKey, folder, now)
    await ctx.db.insert('state', {
      mailboxKey: args.mailboxKey,
      ownerKey: args.ownerKey,
      enabledAt: now,
      seq: 0,
      backfillStatus: 'running',
      backfillRun: 0,
      backfillUpdatedAt: now,
    })
    return { created: true, enabledAt: now }
  },
})

async function ensureFolder(ctx: MutationCtx, mailboxKey: string, folder: Folder, now: number) {
  if (await findFolder(ctx, mailboxKey, folder)) return false
  await ctx.db.insert('folders', {
    mailboxKey,
    folder,
    uidValidity: Math.floor(now / 1000),
    uidNext: 1,
    highestModseq: 1,
    exists: 0,
    unseen: 0,
  })
  return true
}

/**
 * Create further folders for an enabled mailbox (empty, with their own validity). Existing ones
 * are left untouched, including their validity. Returns the newly created ones; `null` if the
 * mailbox is not enabled.
 */
export const addFolders = mutation({
  args: { mailboxKey: v.string(), folders: v.array(folderValidator) },
  returns: v.union(v.null(), v.array(folderValidator)),
  handler: async (ctx, args) => {
    if (!(await findState(ctx, args.mailboxKey))) return null
    const now = Date.now()
    const created: Folder[] = []
    for (const folder of new Set(args.folders)) {
      if (await ensureFolder(ctx, args.mailboxKey, folder, now)) created.push(folder)
    }
    return created
  },
})

/** State of a mailbox, or `null` if it is not enabled. */
export const get = query({
  args: { mailboxKey: v.string() },
  returns: v.union(v.null(), stateValidator),
  handler: async (ctx, args) => {
    const state = await findState(ctx, args.mailboxKey)
    if (!state) return null
    return {
      ownerKey: state.ownerKey,
      enabledAt: state.enabledAt,
      seq: state.seq,
      backfillStatus: state.backfillStatus,
      ...(state.backfillFolder !== undefined ? { backfillFolder: state.backfillFolder } : {}),
      ...(state.backfillCursor !== undefined ? { backfillCursor: state.backfillCursor } : {}),
      backfillRun: state.backfillRun,
      backfillUpdatedAt: state.backfillUpdatedAt,
      ...(state.repairCursor !== undefined ? { repairCursor: state.repairCursor } : {}),
    }
  },
})

/**
 * Advance the backfill state. `folder` and `cursor` replace the previous value (missing =
 * empty); `run` only if given. No mail client sees any of this: `seq` stays.
 */
export const setBackfill = mutation({
  args: {
    mailboxKey: v.string(),
    status: backfillStatusValidator,
    folder: v.optional(folderValidator),
    cursor: v.optional(v.string()),
    run: v.optional(v.number()),
  },
  returns: v.boolean(),
  handler: async (ctx, args) => {
    const state = await findState(ctx, args.mailboxKey)
    if (!state) return false
    await ctx.db.patch('state', state._id, {
      backfillStatus: args.status,
      backfillFolder: args.folder,
      backfillCursor: args.cursor,
      ...(args.run !== undefined ? { backfillRun: args.run } : {}),
      backfillUpdatedAt: Date.now(),
    })
    return true
  },
})

/**
 * Set the bookmark of the catch-up (if `cursor` is missing, it is cleared). As with the backfill,
 * no mail client sees any of this: `seq` stays. Writes only if the value changes.
 */
export const setRepairCursor = mutation({
  args: { mailboxKey: v.string(), cursor: v.optional(v.string()) },
  returns: v.boolean(),
  handler: async (ctx, args) => {
    const state = await findState(ctx, args.mailboxKey)
    if (!state) return false
    if (state.repairCursor !== args.cursor) {
      await ctx.db.patch('state', state._id, { repairCursor: args.cursor })
    }
    return true
  },
})

/** Enabled mailboxes, paged by key (for the daily maintenance). */
export const list = query({
  args: { afterKey: v.optional(v.string()), limit: v.number() },
  returns: v.object({
    rows: v.array(
      v.object({
        mailboxKey: v.string(),
        backfillStatus: backfillStatusValidator,
        backfillUpdatedAt: v.number(),
      }),
    ),
    isDone: v.boolean(),
  }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const rows = await ctx.db
      .query('state')
      .withIndex('by_mailboxKey', (q) => q.gt('mailboxKey', args.afterKey ?? ''))
      .take(limit + 1)
    return {
      rows: rows.slice(0, limit).map((row) => ({
        mailboxKey: row.mailboxKey,
        backfillStatus: row.backfillStatus,
        backfillUpdatedAt: row.backfillUpdatedAt,
      })),
      isDone: rows.length <= limit,
    }
  },
})
