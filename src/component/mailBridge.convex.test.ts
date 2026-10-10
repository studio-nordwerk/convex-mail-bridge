import { convexTest, type TestConvex } from 'convex-test'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import schema from './schema.js'
import { api } from './_generated/api.js'
import { mailBridgeModules } from './test.modules.js'

// The component on its own, loaded as an app: UIDs, counters, tombstones, paging. How an app
// connects to it is covered by the tests of the example app (example/convex).

const DAY = 24 * 60 * 60 * 1000
const A = 'mailbox-a'
const B = 'mailbox-b'

type T = TestConvex<typeof schema>

async function enable(t: T, mailboxKey = A) {
  return await t.mutation(api.mailbox.enable, { mailboxKey, ownerKey: `owner-${mailboxKey}` })
}

async function add(
  t: T,
  messageKey: string,
  overrides: Partial<{
    mailboxKey: string
    folder: 'INBOX' | 'Sent' | 'Archive' | 'Trash' | 'Later'
    groupKey: string
    kind: 'in' | 'out'
    seen: boolean
    size: number
    flags: string[]
  }> = {},
) {
  return await t.mutation(api.entries.add, {
    mailboxKey: A,
    folder: 'INBOX',
    messageKey,
    size: 100,
    internalDate: 1_700_000_000_000,
    seen: false,
    ...overrides,
  })
}

async function folder(t: T, name: 'INBOX' | 'Sent' = 'INBOX', mailboxKey = A) {
  const rows = await t.query(api.folders.list, { mailboxKey })
  const row = rows.find((entry) => entry.folder === name)
  if (!row) throw new Error('folder missing')
  return row
}

async function seq(t: T, mailboxKey = A) {
  const state = await t.query(api.mailbox.get, { mailboxKey })
  if (!state) throw new Error('state missing')
  return state.seq
}

/** State from which "did anything change?" can be read off. */
async function stamp(t: T, mailboxKey = A) {
  return {
    seq: await seq(t, mailboxKey),
    inbox: (await folder(t, 'INBOX', mailboxKey)).highestModseq,
    sent: (await folder(t, 'Sent', mailboxKey)).highestModseq,
  }
}

async function allRows(t: T) {
  return await t.run(async (ctx) => await ctx.db.query('entries').collect())
}

describe('mailBridge (component on its own)', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-07T10:00:00Z'))
    t = convexTest(schema, mailBridgeModules)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('enable creates the state and both folders and is idempotent', async () => {
    const first = await enable(t)
    expect(first.created).toBe(true)
    const folders = await t.query(api.folders.list, { mailboxKey: A })
    expect(folders).toEqual([
      {
        folder: 'INBOX',
        uidValidity: Math.floor(Date.now() / 1000),
        uidNext: 1,
        highestModseq: 1,
        exists: 0,
        unseen: 0,
      },
      {
        folder: 'Sent',
        uidValidity: Math.floor(Date.now() / 1000),
        uidNext: 1,
        highestModseq: 1,
        exists: 0,
        unseen: 0,
      },
    ])
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toMatchObject({
      ownerKey: `owner-${A}`,
      seq: 0,
      backfillStatus: 'running',
      enabledAt: first.enabledAt,
    })

    await add(t, 'm1')
    vi.advanceTimersByTime(5000)
    const second = await t.mutation(api.mailbox.enable, {
      mailboxKey: A,
      ownerKey: 'someone-else',
    })
    expect(second).toEqual({ created: false, enabledAt: first.enabledAt })
    // Nothing reset: UIDs, validity and owner stay.
    expect(await folder(t)).toMatchObject({ uidNext: 2, uidValidity: folders[0].uidValidity })
    expect((await t.query(api.mailbox.get, { mailboxKey: A }))?.ownerKey).toBe(`owner-${A}`)
    expect(
      await t.run(async (ctx) => ({
        folders: (await ctx.db.query('folders').collect()).length,
        states: (await ctx.db.query('state').collect()).length,
      })),
    ).toEqual({ folders: 2, states: 1 })
  })

  it('mailbox not enabled: add returns null and writes nothing', async () => {
    expect(await add(t, 'm1')).toBeNull()
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toBeNull()
    expect(await t.query(api.folders.list, { mailboxKey: A })).toEqual([])
    expect(await allRows(t)).toEqual([])
  })

  it('UIDs increase strictly per folder and mailbox and are never reused', async () => {
    await enable(t, A)
    await enable(t, B)
    expect(await add(t, 'a1')).toEqual({ uid: 1, created: true, moved: false })
    expect(await add(t, 'a2')).toEqual({ uid: 2, created: true, moved: false })
    expect(await add(t, 's1', { folder: 'Sent', seen: true })).toEqual({
      uid: 1,
      created: true,
      moved: false,
    })
    expect(await add(t, 'b1', { mailboxKey: B })).toEqual({ uid: 1, created: true, moved: false })
    expect(await add(t, 'a3')).toEqual({ uid: 3, created: true, moved: false })

    // Removing never frees a UID: neither in the mail client nor permanently, not even after the
    // tombstones are cleaned up.
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [3] })
    await t.mutation(api.entries.remove, { messageKey: 'a2' })
    expect(await add(t, 'a4')).toEqual({ uid: 4, created: true, moved: false })
    vi.advanceTimersByTime(31 * DAY)
    await t.mutation(api.maintenance.cleanup, { before: Date.now() - 30 * DAY, limit: 100 })
    expect(await add(t, 'a5')).toEqual({ uid: 5, created: true, moved: false })

    expect(await folder(t, 'INBOX', A)).toMatchObject({ uidNext: 6, exists: 3, unseen: 3 })
    expect(await folder(t, 'Sent', A)).toMatchObject({ uidNext: 2, exists: 1, unseen: 0 })
    expect(await folder(t, 'INBOX', B)).toMatchObject({ uidNext: 2, exists: 1, unseen: 1 })
    const uids = (await allRows(t))
      .filter((row) => row.mailboxKey === A && row.folder === 'INBOX')
      .map((row) => row.uid)
    expect(new Set(uids).size).toBe(uids.length)
  })

  it('add is idempotent in the same folder, also after hide and remove: the message does not come back', async () => {
    await enable(t)
    const first = await add(t, 'm1', { size: 4711 })
    const before = await stamp(t)
    // Second call with different values: same UID, no write.
    expect(await add(t, 'm1', { size: 1, seen: true })).toEqual({
      uid: first?.uid,
      created: false,
      moved: false,
    })
    expect(await stamp(t)).toEqual(before)
    expect(await allRows(t)).toHaveLength(1)
    expect((await allRows(t))[0]).toMatchObject({ size: 4711, folder: 'INBOX', flags: [] })

    await add(t, 'm2')
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1] })
    await t.mutation(api.entries.remove, { messageKey: 'm2' })
    const afterRemoval = await stamp(t)
    expect(await add(t, 'm1')).toEqual({ uid: 1, created: false, moved: false })
    expect(await add(t, 'm2')).toEqual({ uid: 2, created: false, moved: false })
    expect(await stamp(t)).toEqual(afterRemoval)
    expect(await folder(t)).toMatchObject({ exists: 0, unseen: 0, uidNext: 3 })
    expect((await allRows(t)).map((row) => row.live)).toEqual([false, false])
  })

  it('setSeenMany writes only on change and keeps unseen correct', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2')
    await add(t, 'm3', { seen: true })
    expect(await folder(t)).toMatchObject({ exists: 3, unseen: 2 })

    const before = await stamp(t)
    const changed = await t.mutation(api.entries.setSeenMany, {
      mailboxKey: A,
      changes: [
        { messageKey: 'm1', seen: true },
        { messageKey: 'm3', seen: true }, // already read
        { messageKey: 'does-not-exist', seen: true },
      ],
    })
    expect(changed).toBe(1)
    expect(await folder(t)).toMatchObject({ exists: 3, unseen: 1 })
    const after = await stamp(t)
    expect(after).toEqual({ seq: before.seq + 1, inbox: before.inbox + 1, sent: before.sent })

    // Nothing to change: no write, neither to the folder nor to the state.
    expect(
      await t.mutation(api.entries.setSeenMany, {
        mailboxKey: A,
        changes: [
          { messageKey: 'm1', seen: true },
          { messageKey: 'm2', seen: false },
        ],
      }),
    ).toBe(0)
    expect(await stamp(t)).toEqual(after)

    // Back to unread, and one message twice in the same call: the last state counts.
    expect(
      await t.mutation(api.entries.setSeenMany, {
        mailboxKey: A,
        changes: [
          { messageKey: 'm1', seen: false },
          { messageKey: 'm2', seen: true },
          { messageKey: 'm2', seen: false },
        ],
      }),
    ).toBe(3)
    expect(await folder(t)).toMatchObject({ exists: 3, unseen: 2 })

    // Removed rows and rows of other mailboxes are skipped.
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [2] })
    await enable(t, B)
    await add(t, 'b1', { mailboxKey: B })
    const stable = await stamp(t)
    expect(
      await t.mutation(api.entries.setSeenMany, {
        mailboxKey: A,
        changes: [
          { messageKey: 'm2', seen: true },
          { messageKey: 'b1', seen: true },
        ],
      }),
    ).toBe(0)
    expect(await stamp(t)).toEqual(stable)
    expect(await folder(t, 'INBOX', B)).toMatchObject({ unseen: 1 })
  })

  it('250 changes in one call: every row has its own stamp, changes pages without gaps', async () => {
    await enable(t)
    for (let index = 1; index <= 250; index++) await add(t, `m${index}`)
    const since = (await folder(t)).highestModseq
    const changed = await t.mutation(api.entries.setSeenMany, {
      mailboxKey: A,
      changes: Array.from({ length: 250 }, (_, index) => ({
        messageKey: `m${index + 1}`,
        seen: true,
      })),
    })
    expect(changed).toBe(250)
    expect(await folder(t)).toMatchObject({ exists: 250, unseen: 0, highestModseq: since + 250 })

    const seen: number[] = []
    const modseqs: number[] = []
    let cursor = since
    let pages = 0
    for (;;) {
      const page = await t.query(api.entries.changes, {
        mailboxKey: A,
        folder: 'INBOX',
        sinceModseq: cursor,
        limit: 100,
      })
      pages += 1
      for (const row of page.rows) {
        seen.push(row.uid)
        modseqs.push(row.modseq)
      }
      if (page.isDone) break
      cursor = page.rows[page.rows.length - 1].modseq
    }
    expect(pages).toBe(3)
    expect(seen).toHaveLength(250)
    expect(new Set(seen).size).toBe(250)
    expect(new Set(modseqs).size).toBe(250)
    expect(modseqs).toEqual([...modseqs].sort((a, b) => a - b))
  })

  it('setFlags: flags live in the row, seenChanged reports the read state', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2', { seen: true, flags: ['$Forwarded'] })

    const before = await stamp(t)
    const result = await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1, 2, 2, 99],
      add: ['\\flagged', '\\Seen', '\\Recent'],
      remove: ['$Forwarded'],
    })
    const base = { changed: true, messageKey: 'm1', flags: ['\\Flagged', '\\Seen'] }
    expect(result).toEqual([
      { ...base, uid: 1, modseq: before.inbox + 1, seenChanged: true },
      { ...base, uid: 2, messageKey: 'm2', modseq: before.inbox + 2, seenChanged: false },
    ])
    expect(await folder(t)).toMatchObject({ exists: 2, unseen: 0 })
    // Two changed rows: two own stamps, one step of the change counter.
    expect(await stamp(t)).toEqual({ seq: before.seq + 1, inbox: before.inbox + 2, sent: 1 })

    // Doing the same again changes nothing, but returns the state.
    const again = await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1],
      add: ['\\Flagged'],
      remove: [],
    })
    expect(again).toEqual([
      { ...base, uid: 1, modseq: before.inbox + 1, changed: false, seenChanged: false },
    ])
    expect(await stamp(t)).toEqual({ seq: before.seq + 1, inbox: before.inbox + 2, sent: 1 })

    const unseen = await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1],
      add: [],
      remove: ['\\Seen'],
    })
    expect(unseen).toEqual([
      { ...base, uid: 1, flags: ['\\Flagged'], modseq: before.inbox + 3, seenChanged: true },
    ])
    expect(await folder(t)).toMatchObject({ unseen: 1 })

    // Wrong folder, removed row: nothing.
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [2] })
    expect(
      await t.mutation(api.entries.setFlags, {
        mailboxKey: A,
        folder: 'INBOX',
        uids: [2],
        add: ['x'],
        remove: [],
      }),
    ).toEqual([])
    expect(
      await t.mutation(api.entries.setFlags, {
        mailboxKey: A,
        folder: 'Sent',
        uids: [1],
        add: ['x'],
        remove: [],
      }),
    ).toEqual([])
  })

  it('hide versus remove: the tombstone stays, cleanup deletes only purge rows after the retention period', async () => {
    await enable(t)
    await add(t, 'client') // removed in the mail client
    await add(t, 'purge', { seen: true }) // permanently deleted
    await add(t, 'both') // first removed in the mail client, later permanently deleted
    await add(t, 'stays')

    expect(
      await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1, 3, 1, 42] }),
    ).toEqual([
      { uid: 1, messageKey: 'client' },
      { uid: 3, messageKey: 'both' },
    ])
    expect(await t.mutation(api.entries.remove, { messageKey: 'purge' })).toBe(true)
    expect(await t.mutation(api.entries.remove, { messageKey: 'unknown' })).toBe(false)
    expect(await folder(t)).toMatchObject({ exists: 1, unseen: 1 })

    const byKey = async (messageKey: string) => await t.query(api.entries.byMessage, { messageKey })
    expect(await byKey('client')).toMatchObject({ live: false, removedBy: 'client', uid: 1 })
    expect(await byKey('purge')).toMatchObject({ live: false, removedBy: 'purge', uid: 2 })

    // Still within the retention period: nothing is deleted.
    vi.advanceTimersByTime(10 * DAY)
    expect(
      await t.mutation(api.maintenance.cleanup, { before: Date.now() - 30 * DAY, limit: 100 }),
    ).toEqual({ deleted: 0, isDone: true })

    // The message behind `both` is now permanently deleted: the tombstone becomes `purge`, the
    // retention period starts anew. Nothing changes for mail clients (no new stamp).
    const beforeRelabel = await stamp(t)
    expect(await t.mutation(api.entries.remove, { messageKey: 'both' })).toBe(true)
    expect(await stamp(t)).toEqual(beforeRelabel)
    expect(await byKey('both')).toMatchObject({ live: false, removedBy: 'purge' })

    vi.advanceTimersByTime(21 * DAY)
    expect(
      await t.mutation(api.maintenance.cleanup, { before: Date.now() - 30 * DAY, limit: 100 }),
    ).toEqual({ deleted: 1, isDone: true })
    expect(await byKey('purge')).toBeNull()
    expect(await byKey('both')).not.toBeNull()

    vi.advanceTimersByTime(60 * DAY)
    expect(
      await t.mutation(api.maintenance.cleanup, { before: Date.now() - 30 * DAY, limit: 100 }),
    ).toEqual({ deleted: 1, isDone: true })
    expect(await byKey('both')).toBeNull()
    // The tombstone from the mail client and the live row survive any retention period.
    expect(await byKey('client')).toMatchObject({ live: false, removedBy: 'client' })
    expect(await byKey('stays')).toMatchObject({ live: true })
    expect(await folder(t)).toMatchObject({ exists: 1, unseen: 1, uidNext: 5 })
  })

  it('cleanup works in batches', async () => {
    await enable(t)
    for (let index = 1; index <= 5; index++) {
      await add(t, `m${index}`)
      await t.mutation(api.entries.remove, { messageKey: `m${index}` })
    }
    vi.advanceTimersByTime(31 * DAY)
    const before = Date.now() - 30 * DAY
    expect(await t.mutation(api.maintenance.cleanup, { before, limit: 2 })).toEqual({
      deleted: 2,
      isDone: false,
    })
    expect(await t.mutation(api.maintenance.cleanup, { before, limit: 2 })).toEqual({
      deleted: 2,
      isDone: false,
    })
    expect(await t.mutation(api.maintenance.cleanup, { before, limit: 2 })).toEqual({
      deleted: 1,
      isDone: true,
    })
    expect(await allRows(t)).toEqual([])
  })

  it('changes also returns removed rows, list and scan separate live and removed', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2')
    await add(t, 'm3')
    const since = (await folder(t)).highestModseq
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [2] })
    await t.mutation(api.entries.remove, { messageKey: 'm3' })

    const changes = await t.query(api.entries.changes, {
      mailboxKey: A,
      folder: 'INBOX',
      sinceModseq: since,
      limit: 10,
    })
    expect(changes.isDone).toBe(true)
    expect(
      changes.rows.map((row) => [
        row.uid,
        row.live,
        row.removedBy,
        row.removedModseq === row.modseq,
      ]),
    ).toEqual([
      [2, false, 'client', true],
      [3, false, 'purge', true],
    ])
    // From the start: all three rows, ordered by stamp.
    const all = await t.query(api.entries.changes, {
      mailboxKey: A,
      folder: 'INBOX',
      sinceModseq: 0,
      limit: 10,
    })
    expect(all.rows.map((row) => row.uid)).toEqual([1, 2, 3])

    const live = await t.query(api.entries.list, { mailboxKey: A, folder: 'INBOX', limit: 10 })
    expect(live).toMatchObject({ isDone: true })
    expect(live.rows.map((row) => row.uid)).toEqual([1])

    const scanLive = await t.query(api.maintenance.scan, {
      mailboxKey: A,
      folder: 'INBOX',
      live: true,
      limit: 10,
    })
    const scanDead = await t.query(api.maintenance.scan, {
      mailboxKey: A,
      folder: 'INBOX',
      live: false,
      limit: 10,
    })
    expect(scanLive.rows.map((row) => row.uid)).toEqual([1])
    expect(scanDead.rows.map((row) => [row.uid, row.removedBy])).toEqual([
      [2, 'client'],
      [3, 'purge'],
    ])
  })

  it('list pages over the live rows with afterUid', async () => {
    await enable(t)
    for (let index = 1; index <= 7; index++) await add(t, `m${index}`)
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [3] })
    const uids: number[] = []
    let afterUid: number | undefined
    for (;;) {
      const page = await t.query(api.entries.list, {
        mailboxKey: A,
        folder: 'INBOX',
        limit: 2,
        ...(afterUid !== undefined ? { afterUid } : {}),
      })
      uids.push(...page.rows.map((row) => row.uid))
      if (page.isDone) break
      afterUid = page.rows[page.rows.length - 1].uid
    }
    expect(uids).toEqual([1, 2, 4, 5, 6, 7])
  })

  it('every change raises seq and highestModseq, a call without effect raises neither', async () => {
    await enable(t)
    let last = await stamp(t)
    const expectChange = async (inbox: number, sent = 0) => {
      const now = await stamp(t)
      expect(now).toEqual({ seq: last.seq + 1, inbox: last.inbox + inbox, sent: last.sent + sent })
      last = now
    }
    const expectNoChange = async () => expect(await stamp(t)).toEqual(last)

    await add(t, 'm1')
    await expectChange(1)
    await add(t, 's1', { folder: 'Sent', seen: true })
    await expectChange(0, 1)
    await t.mutation(api.entries.setSeenMany, {
      mailboxKey: A,
      changes: [{ messageKey: 'm1', seen: true }],
    })
    await expectChange(1)
    await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1],
      add: ['$Forwarded'],
      remove: [],
    })
    await expectChange(1)
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1] })
    await expectChange(1)
    await t.mutation(api.entries.remove, { messageKey: 's1' })
    await expectChange(0, 1)

    await add(t, 'm1')
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1] })
    await t.mutation(api.entries.setSeenMany, {
      mailboxKey: A,
      changes: [{ messageKey: 'm1', seen: false }],
    })
    await t.mutation(api.entries.remove, { messageKey: 'unknown' })
    await t.mutation(api.mailbox.setBackfill, { mailboxKey: A, status: 'done' })
    await expectNoChange()

    // The stamp of a row is never above that of the folder, and no value occurs twice.
    const rows = await allRows(t)
    for (const name of ['INBOX', 'Sent'] as const) {
      const modseqs = rows.filter((row) => row.folder === name).map((row) => row.modseq)
      expect(Math.max(...modseqs)).toBeLessThanOrEqual((await folder(t, name)).highestModseq)
      expect(new Set(modseqs).size).toBe(modseqs.length)
    }
  })

  it('two mailboxes do not affect each other', async () => {
    await enable(t, A)
    await enable(t, B)
    await add(t, 'b1', { mailboxKey: B })
    const other = await stamp(t, B)
    const otherFolder = await folder(t, 'INBOX', B)

    await add(t, 'a1')
    await add(t, 'a2')
    await t.mutation(api.entries.setSeenMany, {
      mailboxKey: A,
      changes: [{ messageKey: 'a1', seen: true }],
    })
    // UID 1 exists in both mailboxes: hide and setFlags only hit the one named.
    await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1],
      add: ['x'],
      remove: [],
    })
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1] })
    await t.mutation(api.entries.remove, { messageKey: 'a2' })

    expect(await stamp(t, B)).toEqual(other)
    expect(await folder(t, 'INBOX', B)).toEqual(otherFolder)
    expect(await t.query(api.entries.byMessage, { messageKey: 'b1' })).toMatchObject({
      live: true,
      flags: [],
      uid: 1,
    })
  })

  it('backfill state: advance, read, list mailboxes', async () => {
    expect(await t.mutation(api.mailbox.setBackfill, { mailboxKey: A, status: 'done' })).toBe(false)
    await enable(t, A)
    await enable(t, B)
    expect(
      await t.mutation(api.mailbox.setBackfill, {
        mailboxKey: A,
        status: 'running',
        folder: 'INBOX',
        cursor: 'page-2',
        run: 3,
      }),
    ).toBe(true)
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toMatchObject({
      backfillStatus: 'running',
      backfillFolder: 'INBOX',
      backfillCursor: 'page-2',
      backfillRun: 3,
    })
    // Without folder and cursor: both are empty, the run stays.
    await t.mutation(api.mailbox.setBackfill, { mailboxKey: A, status: 'done' })
    const done = await t.query(api.mailbox.get, { mailboxKey: A })
    expect(done).toMatchObject({ backfillStatus: 'done', backfillRun: 3 })
    expect(done?.backfillFolder).toBeUndefined()
    expect(done?.backfillCursor).toBeUndefined()

    // Bookmark of the catch-up: set, read, clear; `seq` and the backfill stay untouched.
    expect(
      await t.mutation(api.mailbox.setRepairCursor, { mailboxKey: 'missing', cursor: 'x' }),
    ).toBe(false)
    const untouched = await t.query(api.mailbox.get, { mailboxKey: A })
    expect(untouched?.repairCursor).toBeUndefined()
    expect(await t.mutation(api.mailbox.setRepairCursor, { mailboxKey: A, cursor: '17:4' })).toBe(
      true,
    )
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toEqual({
      ...untouched,
      repairCursor: '17:4',
    })
    expect((await t.query(api.mailbox.get, { mailboxKey: B }))?.repairCursor).toBeUndefined()
    expect(await t.mutation(api.mailbox.setRepairCursor, { mailboxKey: A })).toBe(true)
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toEqual(untouched)

    const first = await t.query(api.mailbox.list, { limit: 1 })
    expect(first).toMatchObject({ isDone: false })
    expect(first.rows.map((row) => [row.mailboxKey, row.backfillStatus])).toEqual([[A, 'done']])
    const second = await t.query(api.mailbox.list, { afterKey: A, limit: 1 })
    expect(second).toMatchObject({ isDone: true })
    expect(second.rows.map((row) => [row.mailboxKey, row.backfillStatus])).toEqual([[B, 'running']])
  })

  it('unusable sizes are rejected before anything is written', async () => {
    await enable(t)
    await expect(add(t, 'm1', { size: -1 })).rejects.toThrow('invalid size')
    await expect(add(t, 'm1', { size: Number.NaN })).rejects.toThrow('invalid size')
    expect(await allRows(t)).toEqual([])
    expect(await folder(t)).toMatchObject({ uidNext: 1, exists: 0 })
  })
})
