import { defineSchema, defineTable } from 'convex/server'
import { v } from 'convex/values'

// A deliberately small mail app: mailboxes, their messages, and members of shared mailboxes.
// Everything in it is invented. The component knows none of these tables; the app tells it what
// happened (see mail.ts) and answers its questions (see mailBridge.ts).

export default defineSchema({
  mailboxes: defineTable({
    /** Lowercase. Mail clients sign in with it. */
    address: v.string(),
    /** Whatever identifies a user in your app. */
    ownerId: v.string(),
    disabled: v.boolean(),
  }).index('by_address', ['address']),

  /** People other than the owner who may use a mailbox. */
  mailboxMembers: defineTable({
    mailboxId: v.id('mailboxes'),
    userId: v.string(),
  }).index('by_mailboxId_and_userId', ['mailboxId', 'userId']),

  messages: defineTable({
    mailboxId: v.id('mailboxes'),
    direction: v.union(v.literal('in'), v.literal('out')),
    /** Message-ID header without angle brackets. */
    messageId: v.string(),
    subject: v.string(),
    /** The message as it went over the wire (RFC 822). */
    rawStorageId: v.id('_storage'),
    /** Byte length of the raw message. */
    size: v.number(),
    /** SHA-256 of the raw message, for messages handed in by a mail client. */
    digest: v.optional(v.string()),
    date: v.number(),
    readAt: v.optional(v.number()),
  })
    .index('by_mailboxId_and_date', ['mailboxId', 'date'])
    .index('by_mailboxId_and_messageId', ['mailboxId', 'messageId']),
})
