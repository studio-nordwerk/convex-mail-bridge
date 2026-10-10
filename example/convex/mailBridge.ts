// The connection between this app and the mail bridge component. This is the file you write for
// your own app: one MailBridge instance and the answers to its questions.

import { MailBridge } from '@nordwerk/convex-mail-bridge'
import { components } from './_generated/api.js'
import type { DataModel, Doc } from './_generated/dataModel.js'
import type { QueryCtx } from './_generated/server.js'

export const bridge = new MailBridge<DataModel>(components.mailBridge)

/** The message behind a key, if it belongs to this mailbox. */
async function messageOf(
  ctx: QueryCtx,
  mailboxKey: string,
  messageKey: string,
): Promise<Doc<'messages'> | null> {
  const id = ctx.db.normalizeId('messages', messageKey)
  const message = id ? await ctx.db.get('messages', id) : null
  return message && message.mailboxId === mailboxKey ? message : null
}

export const {
  login,
  seq,
  folders,
  entries,
  changes,
  raw,
  store,
  expunge,
  append,
  submitCheck,
  session,
  cleanup,
} = bridge.edge({
  mailboxByAddress: async (ctx, address) => {
    const mailbox = await ctx.db
      .query('mailboxes')
      .withIndex('by_address', (q) => q.eq('address', address))
      .unique()
    if (!mailbox || mailbox.disabled) return null
    return { mailboxKey: mailbox._id, ownerKey: mailbox.ownerId }
  },

  mailboxOwner: async (ctx, mailboxKey) => {
    const id = ctx.db.normalizeId('mailboxes', mailboxKey)
    const mailbox = id ? await ctx.db.get('mailboxes', id) : null
    return mailbox && !mailbox.disabled ? mailbox.ownerId : null
  },

  // Shared mailboxes: a member may use the mailbox as long as the row exists.
  allowMember: async (ctx, { mailboxKey, userKey }) => {
    const mailboxId = ctx.db.normalizeId('mailboxes', mailboxKey)
    if (!mailboxId) return false
    const member = await ctx.db
      .query('mailboxMembers')
      .withIndex('by_mailboxId_and_userId', (q) => q.eq('mailboxId', mailboxId).eq('userId', userKey))
      .unique()
    return member !== null
  },

  rawLocation: async (ctx, { mailboxKey, messageKey }) => {
    const message = await messageOf(ctx, mailboxKey, messageKey)
    return message?.rawStorageId ?? null
  },

  // A mail client marked messages read or unread: bring `readAt` in line. The ledger is already
  // updated, so this must not call `bridge.setSeen`.
  onSeen: async (ctx, { mailboxKey, messages }) => {
    for (const change of messages) {
      const message = await messageOf(ctx, mailboxKey, change.messageKey)
      if (!message || message.direction !== 'in') continue
      if (change.seen === (message.readAt !== undefined)) continue
      await ctx.db.patch('messages', message._id, { readAt: change.seen ? Date.now() : undefined })
    }
  },

  findSent: async (ctx, { mailboxKey, messageId }) => {
    const mailboxId = ctx.db.normalizeId('mailboxes', mailboxKey)
    if (!mailboxId) return []
    const found = await ctx.db
      .query('messages')
      .withIndex('by_mailboxId_and_messageId', (q) =>
        q.eq('mailboxId', mailboxId).eq('messageId', messageId),
      )
      .take(10)
    return found.filter((message) => message.direction === 'out').map((message) => message._id)
  },
})
