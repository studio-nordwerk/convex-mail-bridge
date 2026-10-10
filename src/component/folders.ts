// Folders of a mailbox with UID and counter state (for the bridge's edge).

import { v } from 'convex/values'
import { query } from './_generated/server.js'
import { folderValidator } from './schema.js'
import { FOLDERS, findFolder } from './lib/ledger.js'

/** The created folders in fixed order; empty if the mailbox is not enabled. */
export const list = query({
  args: { mailboxKey: v.string() },
  returns: v.array(
    v.object({
      folder: folderValidator,
      uidValidity: v.number(),
      uidNext: v.number(),
      highestModseq: v.number(),
      exists: v.number(),
      unseen: v.number(),
    }),
  ),
  handler: async (ctx, args) => {
    const rows = []
    for (const folder of FOLDERS) {
      const doc = await findFolder(ctx, args.mailboxKey, folder)
      if (!doc) continue
      rows.push({
        folder: doc.folder,
        uidValidity: doc.uidValidity,
        uidNext: doc.uidNext,
        highestModseq: doc.highestModseq,
        exists: doc.exists,
        unseen: doc.unseen,
      })
    }
    return rows
  },
})
