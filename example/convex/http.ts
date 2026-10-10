import { httpRouter } from 'convex/server'
import { normalizeMessageId, type SubmitResult } from '@nordwerk/convex-mail-bridge'
import { internal } from './_generated/api.js'
import type { Id } from './_generated/dataModel.js'
import { bridge } from './mailBridge.js'
import { addressOf, headerOf } from './lib/rfc822.js'

const http = httpRouter()

// Mounts POST /bridge/v1/{login,seq,folders,entries,changes,raw,store,expunge,append,submit}.
bridge.registerRoutes(http, {
  functions: internal.mailBridge,

  // Raw messages live in Convex file storage, so the default `loadRaw` is enough.

  // A mail client hands in a message over SMTP. A real app sends it with its mail provider here;
  // the example only records it as sent. Whatever you do, a second call for the same message must
  // not send twice: mail clients retry when the connection breaks.
  //
  // The session was checked before the body was read. The mutation that records the message
  // checks it again (`storeSubmitted`), so nothing is stored for a device password that was
  // revoked in the meantime. An app that really sends records the message as "being sent" in
  // that mutation first, then calls its provider, then marks it sent.
  submit: async (ctx, submission): Promise<SubmitResult> => {
    const mailboxId = submission.mailboxKey as Id<'mailboxes'>
    const mailbox = await ctx.runQuery(internal.mail.getMailbox, { mailboxId })
    if (!mailbox) return { status: 'rejected', reason: 'unavailable' }

    // The device may only send as its own mailbox.
    const from = addressOf(headerOf(submission.raw, 'From'))
    if (submission.envelope.from !== mailbox.address || from !== mailbox.address) {
      return { status: 'rejected', reason: 'sender' }
    }
    const messageId = normalizeMessageId(headerOf(submission.raw, 'Message-ID'))
    const subject = headerOf(submission.raw, 'Subject') ?? ''
    if (!messageId) return { status: 'rejected', reason: 'content' }

    // Same Message-ID again: the same bytes are a retry, other bytes are a conflict.
    const known = await ctx.runQuery(internal.mail.findSubmitted, { mailboxId, messageId })
    if (known) {
      return known.digest === submission.digest
        ? { status: 'sent', messageKey: known.id }
        : { status: 'rejected', reason: 'conflict' }
    }

    const rawStorageId = await ctx.storage.store(
      new Blob([submission.raw], { type: 'message/rfc822' }),
    )
    const messageKey = await ctx.runMutation(internal.mail.storeSubmitted, {
      tokenHash: submission.tokenHash,
      mailboxId,
      messageId,
      subject,
      rawStorageId,
      size: submission.raw.byteLength,
      digest: submission.digest,
      date: Date.now(),
    })
    // The session ended on the way: nothing was stored, the mail client gets `401 session`.
    if (messageKey === null) return { status: 'session' }
    return { status: 'sent', messageKey }
  },
})

export default http
