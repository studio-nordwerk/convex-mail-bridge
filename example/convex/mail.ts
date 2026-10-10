// The app's own mail functions. Wherever a message is stored, read or deleted, the same mutation
// tells the bridge, so that numbering and the app's data commit together.
//
// Everything is `internal` here to keep the example about the bridge. In a real app the functions
// your UI calls are public and check who is signed in before they do anything.

import { v } from 'convex/values'
import { normalizeAddress, normalizeMessageId } from '@nordwerk/convex-mail-bridge'
import { internal } from './_generated/api.js'
import type { Id } from './_generated/dataModel.js'
import {
  internalAction,
  internalMutation,
  internalQuery,
  type MutationCtx,
} from './_generated/server.js'
import { bridge } from './mailBridge.js'
import { buildMessage } from './lib/rfc822.js'

// ─── Mailboxes ──────────────────────────────────────────────────────────

/** Creates a mailbox and opens it for mail clients. */
export const createMailbox = internalMutation({
  args: { address: v.string(), ownerId: v.string() },
  returns: v.id('mailboxes'),
  handler: async (ctx, args) => {
    const address = normalizeAddress(args.address)
    const taken = await ctx.db
      .query('mailboxes')
      .withIndex('by_address', (q) => q.eq('address', address))
      .unique()
    if (taken) throw new Error('Address already in use')
    const mailboxId = await ctx.db.insert('mailboxes', {
      address,
      ownerId: args.ownerId,
      disabled: false,
    })
    await bridge.enable(ctx, { mailboxKey: mailboxId, ownerKey: args.ownerId })
    return mailboxId
  },
})

/** Switches a mailbox off: no sign-in any more, and every device password is revoked. */
export const disableMailbox = internalMutation({
  args: { mailboxId: v.id('mailboxes') },
  returns: v.null(),
  handler: async (ctx, args) => {
    await ctx.db.patch('mailboxes', args.mailboxId, { disabled: true })
    await bridge.revokeAll(ctx, args.mailboxId)
    return null
  },
})

export const addMember = internalMutation({
  args: { mailboxId: v.id('mailboxes'), userId: v.string() },
  returns: v.null(),
  handler: async (ctx, args) => {
    await ctx.db.insert('mailboxMembers', args)
    return null
  },
})

/** Removes a member and, in the same transaction, everything they could sign in with. */
export const removeMember = internalMutation({
  args: { mailboxId: v.id('mailboxes'), userId: v.string() },
  returns: v.null(),
  handler: async (ctx, args) => {
    const member = await ctx.db
      .query('mailboxMembers')
      .withIndex('by_mailboxId_and_userId', (q) =>
        q.eq('mailboxId', args.mailboxId).eq('userId', args.userId),
      )
      .unique()
    if (member) await ctx.db.delete('mailboxMembers', member._id)
    await bridge.revokeMember(ctx, { mailboxKey: args.mailboxId, userKey: args.userId })
    return null
  },
})

// ─── Messages ───────────────────────────────────────────────────────────

const messageFields = {
  mailboxId: v.id('mailboxes'),
  messageId: v.string(),
  subject: v.string(),
  rawStorageId: v.id('_storage'),
  size: v.number(),
  digest: v.optional(v.string()),
  date: v.number(),
}

/** The message document and its UID, in one transaction. */
async function insertMessage(
  ctx: MutationCtx,
  message: {
    mailboxId: Id<'mailboxes'>
    direction: 'in' | 'out'
    messageId: string
    subject: string
    rawStorageId: Id<'_storage'>
    size: number
    digest?: string
    date: number
  },
): Promise<Id<'messages'>> {
  const sent = message.direction === 'out'
  const messageId = await ctx.db.insert('messages', {
    ...message,
    ...(sent ? { readAt: message.date } : {}),
  })
  await bridge.place(ctx, {
    mailboxKey: message.mailboxId,
    messageKey: messageId,
    folder: sent ? 'Sent' : 'INBOX',
    size: message.size,
    internalDate: message.date,
    seen: sent,
  })
  return messageId
}

/**
 * Stores a message and gives it its UID. `size` is the byte length of the raw message in storage:
 * mail clients compare it with what they download.
 */
export const storeMessage = internalMutation({
  args: { ...messageFields, direction: v.union(v.literal('in'), v.literal('out')) },
  returns: v.id('messages'),
  handler: async (ctx, args) => await insertMessage(ctx, args),
})

/**
 * Records a message a mail client handed in over SMTP, or `null` if its session has ended.
 *
 * The route checked the session before it read the body, and that was a while ago. This mutation
 * asks again, in the transaction that writes: a device password revoked or a member removed in
 * between leaves nothing behind, not even the raw file. The mailbox comes from the session.
 */
export const storeSubmitted = internalMutation({
  args: { ...messageFields, tokenHash: v.string() },
  returns: v.union(v.null(), v.id('messages')),
  handler: async (ctx, args): Promise<Id<'messages'> | null> => {
    const { tokenHash, ...message } = args
    const session: { mailboxKey: string } | null = await ctx.runQuery(
      internal.mailBridge.session,
      { tokenHash, now: Date.now() },
    )
    if (!session || session.mailboxKey !== message.mailboxId) {
      await ctx.storage.delete(message.rawStorageId)
      return null
    }
    return await insertMessage(ctx, { ...message, direction: 'out' })
  },
})

/** An invented incoming message: builds the bytes, stores them, records the message. */
export const receive = internalAction({
  args: { mailboxId: v.id('mailboxes'), from: v.string(), subject: v.string(), text: v.string() },
  returns: v.id('messages'),
  handler: async (ctx, args): Promise<Id<'messages'>> => {
    const mailbox = await ctx.runQuery(internal.mail.getMailbox, { mailboxId: args.mailboxId })
    if (!mailbox) throw new Error('Mailbox not found')
    const date = Date.now()
    const messageId = `${crypto.randomUUID()}@example.com`
    const raw = buildMessage({
      from: args.from,
      to: mailbox.address,
      subject: args.subject,
      text: args.text,
      messageId,
      date,
    })
    const rawStorageId = await ctx.storage.store(new Blob([raw], { type: 'message/rfc822' }))
    return await ctx.runMutation(internal.mail.storeMessage, {
      mailboxId: args.mailboxId,
      direction: 'in',
      messageId,
      subject: args.subject,
      rawStorageId,
      size: raw.byteLength,
      date,
    })
  },
})

/** Read state changed in the app (not in a mail client): the app's data and the ledger together. */
export const markRead = internalMutation({
  args: { messageId: v.id('messages'), read: v.boolean() },
  returns: v.null(),
  handler: async (ctx, args) => {
    const message = await ctx.db.get('messages', args.messageId)
    if (!message || message.direction !== 'in') return null
    await ctx.db.patch('messages', message._id, { readAt: args.read ? Date.now() : undefined })
    await bridge.setSeen(ctx, message.mailboxId, [{ messageKey: message._id, seen: args.read }])
    return null
  },
})

/** Deletes a message for good. The ledger is told first, then the document goes. */
export const deleteMessage = internalMutation({
  args: { messageId: v.id('messages') },
  returns: v.null(),
  handler: async (ctx, args) => {
    const message = await ctx.db.get('messages', args.messageId)
    if (!message) return null
    await bridge.remove(ctx, message._id)
    await ctx.storage.delete(message.rawStorageId)
    await ctx.db.delete('messages', message._id)
    return null
  },
})

export const getMailbox = internalQuery({
  args: { mailboxId: v.id('mailboxes') },
  returns: v.union(
    v.null(),
    v.object({ address: v.string(), ownerId: v.string(), disabled: v.boolean() }),
  ),
  handler: async (ctx, args) => {
    const mailbox = await ctx.db.get('mailboxes', args.mailboxId)
    if (!mailbox) return null
    return { address: mailbox.address, ownerId: mailbox.ownerId, disabled: mailbox.disabled }
  },
})

/** A sent message of this mailbox with this Message-ID, for making a resubmission idempotent. */
export const findSubmitted = internalQuery({
  args: { mailboxId: v.id('mailboxes'), messageId: v.string() },
  returns: v.union(v.null(), v.object({ id: v.id('messages'), digest: v.optional(v.string()) })),
  handler: async (ctx, args) => {
    const messageId = normalizeMessageId(args.messageId)
    if (!messageId) return null
    const found = await ctx.db
      .query('messages')
      .withIndex('by_mailboxId_and_messageId', (q) =>
        q.eq('mailboxId', args.mailboxId).eq('messageId', messageId),
      )
      .take(10)
    const sent = found.find((message) => message.direction === 'out')
    return sent ? { id: sent._id, ...(sent.digest !== undefined ? { digest: sent.digest } : {}) } : null
  },
})

// ─── Device passwords ───────────────────────────────────────────────────

/**
 * Creates a device password for one mail client. The password is returned ONCE; only its SHA-256
 * is stored. An action, because the password needs cryptographic randomness.
 */
export const createDevice = internalAction({
  args: { mailboxId: v.id('mailboxes'), userId: v.string(), label: v.string() },
  returns: v.object({ password: v.string() }),
  handler: async (ctx, args): Promise<{ password: string }> => {
    const { password, secretHash } = await bridge.newDevicePassword()
    await ctx.runMutation(internal.mail.insertDevice, { ...args, secretHash })
    return { password }
  },
})

export const insertDevice = internalMutation({
  args: {
    mailboxId: v.id('mailboxes'),
    userId: v.string(),
    label: v.string(),
    secretHash: v.string(),
  },
  returns: v.string(),
  handler: async (ctx, args) => {
    const mailbox = await ctx.db.get('mailboxes', args.mailboxId)
    if (!mailbox || mailbox.disabled) throw new Error('Mailbox not found')
    const result = await bridge.addDevice(ctx, {
      mailboxKey: mailbox._id,
      ownerKey: mailbox.ownerId,
      ...(args.userId !== mailbox.ownerId ? { userKey: args.userId } : {}),
      label: args.label,
      secretHash: args.secretHash,
    })
    if (result.kind !== 'created') throw new Error(`Device not created: ${result.kind}`)
    return result.id
  },
})

export const listDevices = internalQuery({
  args: { mailboxId: v.id('mailboxes'), userId: v.optional(v.string()) },
  returns: v.array(
    v.object({
      id: v.string(),
      label: v.string(),
      createdAt: v.number(),
      lastUsedAt: v.optional(v.number()),
    }),
  ),
  handler: async (ctx, args) => await bridge.listDevices(ctx, args.mailboxId, args.userId),
})

export const revokeDevice = internalMutation({
  args: { mailboxId: v.id('mailboxes'), deviceId: v.string(), userId: v.optional(v.string()) },
  returns: v.boolean(),
  handler: async (ctx, args) =>
    await bridge.revokeDevice(ctx, {
      mailboxKey: args.mailboxId,
      id: args.deviceId,
      ...(args.userId !== undefined ? { userKey: args.userId } : {}),
    }),
})

// ─── Demo data ──────────────────────────────────────────────────────────

/**
 * One mailbox with a few invented messages and one device password, to try a mail client against
 * a development deployment:
 *
 *   npx convex run mail:seed
 *
 * Prints the address and the password (once).
 */
export const seed = internalAction({
  args: {},
  returns: v.object({ address: v.string(), password: v.string() }),
  handler: async (ctx): Promise<{ address: string; password: string }> => {
    const address = 'ada@example.com'
    const mailboxId = await ctx.runMutation(internal.mail.createMailbox, {
      address,
      ownerId: 'user-ada',
    })
    const subjects = ['Welcome', 'Lunch on Thursday?', 'Minutes of the last meeting']
    for (const subject of subjects) {
      await ctx.runAction(internal.mail.receive, {
        mailboxId,
        from: 'Grace <grace@example.com>',
        subject,
        text: `Hello Ada,\n\nthis is an invented message about "${subject}".\n\nGrace`,
      })
    }
    const { password } = await ctx.runAction(internal.mail.createDevice, {
      mailboxId,
      userId: 'user-ada',
      label: 'Demo device',
    })
    return { address, password }
  },
})
