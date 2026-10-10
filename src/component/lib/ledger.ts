// Bookkeeping of the UID ledger: UIDs, change stamp and counters per folder, change counter per
// mailbox. Every component function that changes rows goes through here, so the rules live in
// one place:
//
// - The folder document is read at most once and written at most once per call.
// - Every changed row gets its own change stamp (`highestModseq + 1`); this way
//   `entries.changes` cannot skip anything at a page boundary.
// - `exists` and `unseen` follow the live rows.
// - If the call changed anything, `state.seq` goes up by one.

import type { MutationCtx, QueryCtx } from '../_generated/server.js'
import type { Doc } from '../_generated/dataModel.js'
import { counterDelta } from './flags.js'

export type Folder = Doc<'folders'>['folder']

/** All folders in the order in which `folders.list` returns them. */
export const FOLDERS: readonly Folder[] = ['INBOX', 'Sent', 'Archive', 'Trash', 'Later']

/** The folders that enabling creates; more come with `mailbox.addFolders`. */
export const BASE_FOLDERS: readonly Folder[] = ['INBOX', 'Sent']

/** Page size of the read functions: at least 1, at most this many rows per call. */
export const MAX_PAGE = 1000

export function pageSize(limit: number): number {
  if (!Number.isFinite(limit)) return 1
  return Math.min(Math.max(Math.floor(limit), 1), MAX_PAGE)
}

export async function findState(ctx: QueryCtx, mailboxKey: string) {
  return await ctx.db
    .query('state')
    .withIndex('by_mailboxKey', (q) => q.eq('mailboxKey', mailboxKey))
    .unique()
}

export async function findFolder(ctx: QueryCtx, mailboxKey: string, folder: Folder) {
  return await ctx.db
    .query('folders')
    .withIndex('by_mailboxKey_and_folder', (q) =>
      q.eq('mailboxKey', mailboxKey).eq('folder', folder),
    )
    .unique()
}

/**
 * The current row of a message: the live one, else its tombstone `client` or `purge`. `move`
 * tombstones do not count, they are only the trail of earlier folders. There is at most one per
 * message.
 */
export async function findEntry(ctx: QueryCtx, messageKey: string) {
  for (const removedBy of [undefined, 'client', 'purge'] as const) {
    const entry = await ctx.db
      .query('entries')
      .withIndex('by_messageKey_and_removedBy', (q) =>
        q.eq('messageKey', messageKey).eq('removedBy', removedBy),
      )
      .unique()
    if (entry) return entry
  }
  return null
}

export async function findLiveEntry(
  ctx: QueryCtx,
  mailboxKey: string,
  folder: Folder,
  uid: number,
) {
  return await ctx.db
    .query('entries')
    .withIndex('by_mailboxKey_and_folder_and_live_and_uid', (q) =>
      q.eq('mailboxKey', mailboxKey).eq('folder', folder).eq('live', true).eq('uid', uid),
    )
    .unique()
}

type OpenFolder = {
  doc: Doc<'folders'>
  uidNext: number
  highestModseq: number
  exists: number
  unseen: number
  dirty: boolean
}

type RowState = { seen: boolean } | null

/** Bookkeeping for one call and one mailbox. Call `commit()` exactly once at the end. */
export function openLedger(ctx: MutationCtx, mailboxKey: string) {
  const open = new Map<Folder, OpenFolder>()

  async function folder(name: Folder): Promise<OpenFolder> {
    const cached = open.get(name)
    if (cached) return cached
    const doc = await findFolder(ctx, mailboxKey, name)
    if (!doc) throw new Error(`mailBridge: folder ${name} missing`)
    const loaded = {
      doc,
      uidNext: doc.uidNext,
      highestModseq: doc.highestModseq,
      exists: doc.exists,
      unseen: doc.unseen,
      dirty: false,
    }
    open.set(name, loaded)
    return loaded
  }

  return {
    /** Next UID in the folder; strictly increasing and never reused. */
    async takeUid(name: Folder): Promise<number> {
      const target = await folder(name)
      const uid = target.uidNext
      target.uidNext += 1
      target.dirty = true
      return uid
    },

    /**
     * A row has changed: returns its new, own change stamp and updates the counters.
     * `before`/`after` = state of the row while it is live, else `null`.
     */
    async change(name: Folder, before: RowState, after: RowState): Promise<number> {
      const target = await folder(name)
      const delta = counterDelta(before, after)
      target.highestModseq += 1
      target.exists += delta.exists
      target.unseen += delta.unseen
      target.dirty = true
      return target.highestModseq
    },

    /** Writes each changed folder document once and raises `state.seq` if there was one. */
    async commit(): Promise<boolean> {
      let changed = false
      for (const target of open.values()) {
        if (!target.dirty) continue
        changed = true
        await ctx.db.patch('folders', target.doc._id, {
          uidNext: target.uidNext,
          highestModseq: target.highestModseq,
          exists: target.exists,
          unseen: target.unseen,
        })
      }
      if (!changed) return false
      const state = await findState(ctx, mailboxKey)
      if (state) await ctx.db.patch('state', state._id, { seq: state.seq + 1 })
      return true
    },
  }
}
