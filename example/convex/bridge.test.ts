// The example app at its real HTTP edge (`t.fetch`): the routes of docs/edge-contract.md, called
// the way the bridge calls them, with invented data.

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { internal } from './_generated/api.js'
import type { Id } from './_generated/dataModel.js'
import { buildMessage } from './lib/rfc822.js'
import { initConvexTest } from './setup.test.js'

const SECRET = 'example-shared-secret-0123456789abcdef'
const IP = '192.0.2.10'

type T = ReturnType<typeof initConvexTest>
type Box = { mailboxId: Id<'mailboxes'>; address: string; ownerId: string; password: string }
type Row = { uid: number; flags: string[]; modseq: number; size: number; internalDate: number }
type Page = { rows: Row[]; more: boolean; uidValidity: number; uidNext: number; highestModseq: number }
type FolderInfo = { name: string; specialUse?: string; exists: number; unseen: number; uidValidity: number }

async function post(t: T, route: string, body: unknown, session?: string, secret: string | null = SECRET) {
  return await t.fetch(`/bridge/v1/${route}`, {
    method: 'POST',
    headers: {
      ...(secret !== null ? { Authorization: `Bearer ${secret}` } : {}),
      'Content-Type': 'application/json',
      ...(session ? { 'X-Bridge-Session': session } : {}),
    },
    body: JSON.stringify(body),
  })
}

async function openMailbox(t: T, address = 'ada@example.com', ownerId = 'user-ada'): Promise<Box> {
  const mailboxId = await t.mutation(internal.mail.createMailbox, { address, ownerId })
  const { password } = await t.action(internal.mail.createDevice, {
    mailboxId,
    userId: ownerId,
    label: 'Laptop',
  })
  return { mailboxId, address, ownerId, password }
}

async function receive(t: T, box: Box, subject = 'Hello') {
  return await t.action(internal.mail.receive, {
    mailboxId: box.mailboxId,
    from: 'Grace <grace@example.com>',
    subject,
    text: `An invented message about ${subject}.`,
  })
}

async function login(t: T, box: Pick<Box, 'address' | 'password'>, extra: Record<string, unknown> = {}) {
  return await post(t, 'login', { address: box.address, password: box.password, clientIp: IP, ...extra })
}

async function signIn(t: T, box: Pick<Box, 'address' | 'password'>) {
  const response = await login(t, box)
  expect(response.status).toBe(200)
  return (await response.json()) as { session: string; expiresAt: number; mailbox: string }
}

async function entries(t: T, session: string, folder: string) {
  const response = await post(t, 'entries', { folder, limit: 2000 }, session)
  expect(response.status).toBe(200)
  return (await response.json()) as Page
}

async function folders(t: T, session: string) {
  const response = await post(t, 'folders', {}, session)
  expect(response.status).toBe(200)
  return ((await response.json()) as { folders: FolderInfo[] }).folders
}

async function submit(t: T, session: string, raw: Uint8Array<ArrayBuffer>, from: string, to: string[]) {
  return await t.fetch('/bridge/v1/submit', {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${SECRET}`,
      'Content-Type': 'message/rfc822',
      'X-Bridge-Session': session,
      'X-Envelope-From': from,
      'X-Envelope-To': JSON.stringify(to),
    },
    body: raw,
  })
}

function outgoing(box: Box, messageId: string, text = 'See you there.') {
  return buildMessage({
    from: `Ada <${box.address}>`,
    to: 'grace@example.com',
    subject: 'Re: Lunch',
    text,
    messageId,
    date: Date.now(),
  })
}

const ROUTES_WITH_SESSION: Record<string, unknown> = {
  folders: {},
  entries: { folder: 'INBOX', limit: 10 },
  changes: { folder: 'INBOX', sinceModseq: 0, limit: 10 },
  raw: { folder: 'INBOX', uid: 1, uidValidity: 1 },
  store: { folder: 'INBOX', uids: [1], add: ['\\Seen'], remove: [] },
  expunge: { folder: 'INBOX', uids: [1] },
  append: { folder: 'Sent', messageId: '<x@example.com>', digest: 'a'.repeat(64) },
}

describe('edge of the example app', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-15T10:00:00Z'))
    vi.stubEnv('MAIL_BRIDGE_SECRET', SECRET)
    t = initConvexTest()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllEnvs()
    vi.restoreAllMocks()
  })

  describe('shared secret before anything else', () => {
    test('without or with a wrong secret every route answers 401 with an empty body', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      const routes = { login: {}, seq: { sessions: [] }, submit: {}, ...ROUTES_WITH_SESSION }
      for (const [route, body] of Object.entries(routes)) {
        for (const secret of [null, 'wrong-secret-0123456789abcdef0123456789']) {
          const response = await post(t, route, body, session, secret)
          expect(response.status, route).toBe(401)
          expect(await response.text(), route).toBe('')
        }
      }
    })

    test('a secret shorter than 32 characters switches the edge off', async () => {
      const box = await openMailbox(t)
      vi.stubEnv('MAIL_BRIDGE_SECRET', 'too-short')
      const response = await post(t, 'login', { address: box.address, password: box.password, clientIp: IP }, undefined, 'too-short')
      expect(response.status).toBe(401)
    })
  })

  describe('login', () => {
    test('a device password signs in and gets a session for its mailbox', async () => {
      const box = await openMailbox(t)
      const result = await signIn(t, box)
      expect(result.session).toMatch(/^[A-Za-z0-9_-]{43}$/)
      expect(result.mailbox).toBe(box.mailboxId)
      expect(result.expiresAt).toBe(Date.now() + 12 * 60 * 60 * 1000)
      const devices = await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId })
      expect(devices).toHaveLength(1)
      expect(devices[0].lastUsedAt).toBe(Date.now())
    })

    test('wrong password, unknown address and a disabled mailbox all answer 401 auth', async () => {
      const box = await openMailbox(t)
      const wrong = await login(t, { address: box.address, password: 'mb_wrong' })
      const unknown = await login(t, { address: 'nobody@example.com', password: box.password })
      await t.mutation(internal.mail.disableMailbox, { mailboxId: box.mailboxId })
      const disabled = await login(t, box)
      for (const response of [wrong, unknown, disabled]) {
        expect(response.status).toBe(401)
        expect(await response.json()).toEqual({ error: 'auth', message: 'Sign-in failed.' })
      }
    })

    test('after ten failed attempts a further one is throttled, a valid password never is', async () => {
      const box = await openMailbox(t)
      for (let attempt = 0; attempt < 10; attempt++) {
        expect((await login(t, { address: box.address, password: `mb_wrong${attempt}` })).status).toBe(401)
      }
      const throttled = await login(t, { address: box.address, password: 'mb_another' })
      expect(throttled.status).toBe(429)
      expect(Number(throttled.headers.get('Retry-After'))).toBeGreaterThan(0)
      const body = (await throttled.json()) as { error: string; retryAfterSeconds: number }
      expect(body.error).toBe('throttled')
      expect(body.retryAfterSeconds).toBeGreaterThan(0)
      expect((await login(t, box)).status).toBe(200)
    })

    test('unknown fields, a missing client IP and oversized bodies are refused', async () => {
      const box = await openMailbox(t)
      const extra = await login(t, box, { mailboxKey: box.mailboxId })
      expect(extra.status).toBe(400)
      expect(await extra.json()).toEqual({ error: 'request', message: 'Invalid request.' })
      const noIp = await post(t, 'login', { address: box.address, password: box.password })
      expect(noIp.status).toBe(400)
      const large = await post(t, 'login', { address: 'a'.repeat(70 * 1024), password: 'x', clientIp: IP })
      expect(large.status).toBe(413)
    })

    test('a mailbox that is still being filled answers 503 preparing', async () => {
      const mailboxId = await t.run(async (ctx) =>
        await ctx.db.insert('mailboxes', { address: 'new@example.com', ownerId: 'user-new', disabled: false }),
      )
      await t.run(async (ctx) => {
        const { bridge } = await import('./mailBridge.js')
        await bridge.enable(ctx, { mailboxKey: mailboxId, ownerKey: 'user-new', backfill: true })
      })
      const { password } = await t.action(internal.mail.createDevice, { mailboxId, userId: 'user-new', label: 'Phone' })
      const box = { address: 'new@example.com', password }
      const preparing = await login(t, box)
      expect(preparing.status).toBe(503)
      expect(((await preparing.json()) as { error: string }).error).toBe('preparing')
      await t.run(async (ctx) => {
        const { bridge } = await import('./mailBridge.js')
        await bridge.markReady(ctx, mailboxId)
      })
      expect((await login(t, box)).status).toBe(200)
    })
  })

  describe('reading', () => {
    test('folders, entries and raw agree with what the app stored', async () => {
      const box = await openMailbox(t)
      const first = await receive(t, box, 'One')
      await receive(t, box, 'Two')
      const { session } = await signIn(t, box)

      const list = await folders(t, session)
      expect(list.map((folder) => folder.name)).toEqual(['INBOX', 'Sent'])
      expect(list[0]).toMatchObject({ exists: 2, unseen: 2 })
      expect(list[1]).toMatchObject({ specialUse: '\\Sent', exists: 0 })

      const page = await entries(t, session, 'INBOX')
      expect(page.rows.map((row) => row.uid)).toEqual([1, 2])
      expect(page.more).toBe(false)
      expect(page.uidNext).toBe(3)
      expect(page.rows[0].flags).toEqual([])
      // The key of the message never leaves the edge.
      expect(Object.keys(page.rows[0]).sort()).toEqual(['flags', 'internalDate', 'modseq', 'size', 'uid'])

      const response = await post(t, 'raw', { folder: 'INBOX', uid: 1, uidValidity: page.uidValidity }, session)
      expect(response.status).toBe(200)
      expect(response.headers.get('Content-Type')).toBe('message/rfc822')
      const bytes = new Uint8Array(await response.arrayBuffer())
      expect(bytes.byteLength).toBe(page.rows[0].size)
      expect(Number(response.headers.get('Content-Length'))).toBe(page.rows[0].size)
      expect(new TextDecoder().decode(bytes)).toContain('Subject: One')

      const stored = await t.run(async (ctx) => await ctx.db.get('messages', first))
      expect(stored?.size).toBe(page.rows[0].size)
    })

    test('paging, unknown folders, a stale UIDVALIDITY and missing messages', async () => {
      const box = await openMailbox(t)
      for (const subject of ['One', 'Two', 'Three']) await receive(t, box, subject)
      const { session } = await signIn(t, box)

      const firstPage = await post(t, 'entries', { folder: 'INBOX', limit: 2 }, session)
      const first = (await firstPage.json()) as Page
      expect(first.rows.map((row) => row.uid)).toEqual([1, 2])
      expect(first.more).toBe(true)
      const second = (await (await post(t, 'entries', { folder: 'INBOX', limit: 2, afterUid: 2 }, session)).json()) as Page
      expect(second.rows.map((row) => row.uid)).toEqual([3])
      expect(second.more).toBe(false)

      const unknown = await post(t, 'entries', { folder: 'Archive', limit: 10 }, session)
      expect(unknown.status).toBe(404)
      expect(((await unknown.json()) as { error: string }).error).toBe('folder')

      const stale = await post(t, 'raw', { folder: 'INBOX', uid: 1, uidValidity: first.uidValidity + 1 }, session)
      expect(stale.status).toBe(409)
      expect(((await stale.json()) as { error: string }).error).toBe('uidvalidity')
      const gone = await post(t, 'raw', { folder: 'INBOX', uid: 99, uidValidity: first.uidValidity }, session)
      expect(gone.status).toBe(404)
      expect(((await gone.json()) as { error: string }).error).toBe('gone')

      const future = await post(t, 'changes', { folder: 'INBOX', sinceModseq: first.highestModseq + 100, limit: 10 }, session)
      expect(future.status).toBe(410)
    })

    test('one mailbox never sees the rows of another', async () => {
      const ada = await openMailbox(t)
      const grace = await openMailbox(t, 'grace@example.com', 'user-grace')
      await receive(t, ada, 'For Ada')
      const { session } = await signIn(t, grace)
      expect((await entries(t, session, 'INBOX')).rows).toEqual([])
      const adaPage = await entries(t, (await signIn(t, ada)).session, 'INBOX')
      const response = await post(t, 'raw', { folder: 'INBOX', uid: 1, uidValidity: adaPage.uidValidity }, session)
      expect(response.status).not.toBe(200)
    })
  })

  describe('changes from the mail client', () => {
    test('\\Seen reaches the app in the same step, other flags stay in the ledger', async () => {
      const box = await openMailbox(t)
      const messageId = await receive(t, box)
      const { session } = await signIn(t, box)
      const before = await entries(t, session, 'INBOX')

      const response = await post(t, 'store', { folder: 'INBOX', uids: [1], add: ['\\Seen', '\\Flagged'], remove: [] }, session)
      expect(response.status).toBe(200)
      const { rows } = (await response.json()) as { rows: Array<{ uid: number; flags: string[]; modseq: number }> }
      expect(rows).toHaveLength(1)
      expect(rows[0].flags).toEqual(['\\Flagged', '\\Seen'])
      expect(rows[0].modseq).toBeGreaterThan(before.rows[0].modseq)

      const message = await t.run(async (ctx) => await ctx.db.get('messages', messageId))
      expect(message?.readAt).toBe(Date.now())
      expect((await folders(t, session))[0]).toMatchObject({ exists: 1, unseen: 0 })

      // The same request again changes nothing and reports nothing.
      const again = await post(t, 'store', { folder: 'INBOX', uids: [1], add: ['\\Seen'], remove: [] }, session)
      expect(await again.json()).toEqual({ rows: [] })

      const changed = await post(t, 'changes', { folder: 'INBOX', sinceModseq: before.highestModseq, limit: 10 }, session)
      const page = (await changed.json()) as { rows: Array<Row & { live: boolean }> }
      expect(page.rows).toHaveLength(1)
      expect(page.rows[0]).toMatchObject({ uid: 1, live: true })

      await post(t, 'store', { folder: 'INBOX', uids: [1], add: [], remove: ['\\Seen'] }, session)
      const unread = await t.run(async (ctx) => await ctx.db.get('messages', messageId))
      expect(unread?.readAt).toBeUndefined()
    })

    test('a change in the app shows up for mail clients and moves the change counter', async () => {
      const box = await openMailbox(t)
      const messageId = await receive(t, box)
      const { session } = await signIn(t, box)
      const seqBefore = (await (await post(t, 'seq', { sessions: [session] })).json()) as { results: Array<{ ok: boolean; seq?: number }> }
      expect(seqBefore.results[0].ok).toBe(true)

      await t.mutation(internal.mail.markRead, { messageId, read: true })
      expect((await entries(t, session, 'INBOX')).rows[0].flags).toEqual(['\\Seen'])
      const seqAfter = (await (await post(t, 'seq', { sessions: [session, 'not-a-token'] })).json()) as { results: Array<{ ok: boolean; seq?: number }> }
      expect(seqAfter.results[0].seq).toBeGreaterThan(seqBefore.results[0].seq ?? 0)
      expect(seqAfter.results[1]).toEqual({ ok: false })
    })

    test('expunge hides a message for mail clients and keeps it in the app', async () => {
      const box = await openMailbox(t)
      const messageId = await receive(t, box)
      await receive(t, box, 'Stays')
      const { session } = await signIn(t, box)
      const before = await entries(t, session, 'INBOX')

      const response = await post(t, 'expunge', { folder: 'INBOX', uids: [1, 77] }, session)
      expect(await response.json()).toEqual({ uids: [1] })
      expect((await entries(t, session, 'INBOX')).rows.map((row) => row.uid)).toEqual([2])
      const changed = (await (await post(t, 'changes', { folder: 'INBOX', sinceModseq: before.highestModseq, limit: 10 }, session)).json()) as { rows: Array<Row & { live: boolean }> }
      expect(changed.rows).toMatchObject([{ uid: 1, live: false }])
      expect(await t.run(async (ctx) => await ctx.db.get('messages', messageId))).not.toBeNull()
    })

    test('a message deleted in the app disappears for mail clients', async () => {
      const box = await openMailbox(t)
      const messageId = await receive(t, box)
      const { session } = await signIn(t, box)
      const before = await entries(t, session, 'INBOX')
      await t.mutation(internal.mail.deleteMessage, { messageId })
      expect((await entries(t, session, 'INBOX')).rows).toEqual([])
      const changed = (await (await post(t, 'changes', { folder: 'INBOX', sinceModseq: before.highestModseq, limit: 10 }, session)).json()) as { rows: Array<Row & { live: boolean }> }
      expect(changed.rows).toMatchObject([{ uid: 1, live: false }])
      const raw = await post(t, 'raw', { folder: 'INBOX', uid: 1, uidValidity: before.uidValidity }, session)
      expect(raw.status).toBe(404)
    })
  })

  describe('sending', () => {
    test('a submitted message lands in Sent, and the client copy resolves to it', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      const raw = outgoing(box, 'lunch-1@example.com')

      const response = await submit(t, session, raw, box.address, ['grace@example.com'])
      expect(response.status).toBe(200)
      expect(await response.json()).toEqual({ status: 'sent', uid: 1 })

      const sent = await entries(t, session, 'Sent')
      expect(sent.rows).toHaveLength(1)
      expect(sent.rows[0]).toMatchObject({ uid: 1, flags: ['\\Seen'], size: raw.byteLength })
      const stored = await post(t, 'raw', { folder: 'Sent', uid: 1, uidValidity: sent.uidValidity }, session)
      expect(new Uint8Array(await stored.arrayBuffer())).toEqual(raw)

      // The mail client appends its own copy: it gets the UID of the message that is there.
      const digest = 'b'.repeat(64)
      const append = await post(t, 'append', { folder: 'Sent', messageId: '<lunch-1@example.com>', digest }, session)
      expect(await append.json()).toEqual({ uid: 1, uidValidity: sent.uidValidity })
      const unknown = await post(t, 'append', { folder: 'Sent', messageId: '<other@example.com>', digest }, session)
      expect(unknown.status).toBe(409)
      const inbox = await post(t, 'append', { folder: 'INBOX', messageId: '<lunch-1@example.com>', digest }, session)
      expect(inbox.status).toBe(403)
    })

    test('a retry does not send twice, other bytes under the same id are a conflict', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      const raw = outgoing(box, 'retry@example.com')
      await submit(t, session, raw, box.address, ['grace@example.com'])
      const retry = await submit(t, session, raw, box.address, ['grace@example.com'])
      expect(await retry.json()).toEqual({ status: 'sent', uid: 1 })
      expect((await entries(t, session, 'Sent')).rows).toHaveLength(1)

      const other = outgoing(box, 'retry@example.com', 'Something else entirely.')
      const conflict = await submit(t, session, other, box.address, ['grace@example.com'])
      expect(conflict.status).toBe(409)
      expect(await conflict.json()).toMatchObject({ error: 'conflict', smtpCode: 550 })
    })

    test('a foreign sender, a missing envelope and an oversized message are refused', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      const raw = outgoing(box, 'sender@example.com')

      const foreign = await submit(t, session, raw, 'mallory@example.com', ['grace@example.com'])
      expect(foreign.status).toBe(422)
      expect(await foreign.json()).toMatchObject({ error: 'sender', smtpCode: 550 })

      const noEnvelope = await t.fetch('/bridge/v1/submit', {
        method: 'POST',
        headers: { Authorization: `Bearer ${SECRET}`, 'X-Bridge-Session': session },
        body: raw,
      })
      expect(noEnvelope.status).toBe(400)

      const large = await submit(t, session, new Uint8Array(5 * 1024 * 1024 + 1), box.address, ['grace@example.com'])
      expect(large.status).toBe(413)
      expect(await large.json()).toMatchObject({ error: 'size', smtpCode: 552 })
      expect((await entries(t, session, 'Sent')).rows).toEqual([])
    })

    test('a device password revoked while the body is read stores nothing and ends the session', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      const raw = outgoing(box, 'late@example.com')
      const [device] = await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId })

      // The body is a stream that hands out nothing before it is asked (`highWaterMark: 0`). The
      // route asks only after its early session check passed, and exactly then the device
      // password goes: what is left to stop the message is the check in the storing mutation.
      let revoked = false
      const body = new ReadableStream<Uint8Array>(
        {
          async pull(controller) {
            if (revoked) return controller.close()
            revoked = await t.mutation(internal.mail.revokeDevice, {
              mailboxId: box.mailboxId,
              deviceId: device.id,
            })
            controller.enqueue(raw)
          },
        },
        { highWaterMark: 0 },
      )
      const response = await t.fetch('/bridge/v1/submit', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${SECRET}`,
          'Content-Type': 'message/rfc822',
          'X-Bridge-Session': session,
          'X-Envelope-From': box.address,
          'X-Envelope-To': JSON.stringify(['grace@example.com']),
        },
        body,
        duplex: 'half',
      } as RequestInit)

      expect(revoked).toBe(true)
      expect(response.status).toBe(401)
      expect(await response.json()).toEqual({ error: 'session', message: 'Session invalid or expired.' })
      // Neither a message, nor a row in Sent, nor the raw file.
      const left = await t.run(async (ctx) => {
        const { bridge } = await import('./mailBridge.js')
        return {
          messages: await ctx.db.query('messages').collect(),
          files: await ctx.db.system.query('_storage').collect(),
          sent: (await bridge.folders(ctx, box.mailboxId)).find((folder) => folder.folder === 'Sent'),
        }
      })
      expect(left.messages).toEqual([])
      expect(left.files).toEqual([])
      expect(left.sent).toMatchObject({ exists: 0, uidNext: 1 })
    })
  })

  describe('a session ends when access ends', () => {
    async function expectSessionGone(session: string) {
      for (const [route, body] of Object.entries(ROUTES_WITH_SESSION)) {
        const response = await post(t, route, body, session)
        expect(response.status, route).toBe(401)
        expect(((await response.json()) as { error: string }).error, route).toBe('session')
      }
      const seq = (await (await post(t, 'seq', { sessions: [session] })).json()) as { results: unknown[] }
      expect(seq.results).toEqual([{ ok: false }])
    }

    test('revoking the device password', async () => {
      const box = await openMailbox(t)
      await receive(t, box)
      const { session } = await signIn(t, box)
      const [device] = await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId })
      expect(await t.mutation(internal.mail.revokeDevice, { mailboxId: box.mailboxId, deviceId: device.id })).toBe(true)
      await expectSessionGone(session)
      expect((await login(t, box)).status).toBe(401)
    })

    test('switching the mailbox off', async () => {
      const box = await openMailbox(t)
      await receive(t, box)
      const { session } = await signIn(t, box)
      await t.mutation(internal.mail.disableMailbox, { mailboxId: box.mailboxId })
      await expectSessionGone(session)
    })

    test('handing the mailbox to another owner', async () => {
      const box = await openMailbox(t)
      await receive(t, box)
      const { session } = await signIn(t, box)
      await t.run(async (ctx) => await ctx.db.patch('mailboxes', box.mailboxId, { ownerId: 'user-grace' }))
      await expectSessionGone(session)
      expect((await login(t, box)).status).toBe(401)
    })

    test('expiry after twelve hours, and a malformed token', async () => {
      const box = await openMailbox(t)
      const { session } = await signIn(t, box)
      expect((await post(t, 'folders', {}, session)).status).toBe(200)
      vi.setSystemTime(Date.now() + 12 * 60 * 60 * 1000)
      expect((await post(t, 'folders', {}, session)).status).toBe(401)
      expect((await post(t, 'folders', {}, 'short')).status).toBe(401)
    })
  })

  describe('shared mailboxes', () => {
    async function memberDevice(box: Box, userId: string) {
      const { password } = await t.action(internal.mail.createDevice, {
        mailboxId: box.mailboxId,
        userId,
        label: 'Phone',
      })
      return { address: box.address, password }
    }

    test('a member signs in with their own device password', async () => {
      const box = await openMailbox(t)
      await receive(t, box)
      await t.mutation(internal.mail.addMember, { mailboxId: box.mailboxId, userId: 'user-grace' })
      const member = await memberDevice(box, 'user-grace')
      const { session } = await signIn(t, member)
      expect((await entries(t, session, 'INBOX')).rows).toHaveLength(1)
      // Each person only sees their own devices.
      expect(await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId })).toHaveLength(1)
      expect(
        await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId, userId: 'user-grace' }),
      ).toHaveLength(1)
    })

    test('membership is asked on every request: without it the session and the password are dead', async () => {
      const box = await openMailbox(t)
      await t.mutation(internal.mail.addMember, { mailboxId: box.mailboxId, userId: 'user-grace' })
      const member = await memberDevice(box, 'user-grace')
      const { session } = await signIn(t, member)
      // Only the membership goes; the device password is NOT revoked.
      await t.run(async (ctx) => {
        for (const row of await ctx.db.query('mailboxMembers').collect()) {
          await ctx.db.delete('mailboxMembers', row._id)
        }
      })
      expect((await post(t, 'folders', {}, session)).status).toBe(401)
      const seq = (await (await post(t, 'seq', { sessions: [session] })).json()) as { results: unknown[] }
      expect(seq.results).toEqual([{ ok: false }])
      expect((await login(t, member)).status).toBe(401)
      // The owner is untouched.
      expect((await login(t, box)).status).toBe(200)
    })

    test('removing a member revokes their device passwords in the same step', async () => {
      const box = await openMailbox(t)
      await t.mutation(internal.mail.addMember, { mailboxId: box.mailboxId, userId: 'user-grace' })
      const member = await memberDevice(box, 'user-grace')
      await signIn(t, member)
      await t.mutation(internal.mail.removeMember, { mailboxId: box.mailboxId, userId: 'user-grace' })
      expect(
        await t.query(internal.mail.listDevices, { mailboxId: box.mailboxId, userId: 'user-grace' }),
      ).toEqual([])
      // Even if they were added again, the old password stays revoked.
      await t.mutation(internal.mail.addMember, { mailboxId: box.mailboxId, userId: 'user-grace' })
      expect((await login(t, member)).status).toBe(401)
    })
  })

  describe('folders beyond INBOX and Sent (contract revision 1.2, in progress)', () => {
    test('such a mailbox only takes a bridge that announces version 2', async () => {
      const box = await openMailbox(t)
      await receive(t, box)
      await t.run(async (ctx) => {
        const { bridge } = await import('./mailBridge.js')
        await bridge.addFolders(ctx, box.mailboxId, ['Archive', 'Trash'])
      })
      const old = await login(t, box)
      expect(old.status).toBe(503)
      const current = await login(t, box, { bridge: 2 })
      expect(current.status).toBe(200)
      const { session } = (await current.json()) as { session: string }
      expect((await folders(t, session)).map((folder) => folder.name)).toEqual(['INBOX', 'Sent', 'Archive', 'Trash'])
      // Until /move exists, deleting in INBOX does nothing in such a mailbox.
      expect(await (await post(t, 'expunge', { folder: 'INBOX', uids: [1] }, session)).json()).toEqual({ uids: [] })
      expect((await post(t, 'move', { folder: 'INBOX', uids: [1], target: 'Trash' }, session)).status).toBe(404)
    })
  })

  test('cleanup removes expired sessions', async () => {
    const box = await openMailbox(t)
    await signIn(t, box)
    vi.setSystemTime(Date.now() + 13 * 60 * 60 * 1000)
    const result = await t.mutation(internal.mailBridge.cleanup, {})
    expect(result).toMatchObject({ sessions: 1, isDone: true })
  })

  test('seed creates a mailbox a mail client can sign in to', async () => {
    const { address, password } = await t.action(internal.mail.seed, {})
    const { session } = await signIn(t, { address, password })
    expect((await entries(t, session, 'INBOX')).rows).toHaveLength(3)
  })
})
