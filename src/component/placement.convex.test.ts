import { convexTest, type TestConvex } from 'convex-test'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import schema from './schema.js'
import { api } from './_generated/api.js'
import { mailBridgeModules } from './test.modules.js'

// The component alone: additional folders, folder changes with a new UID, moving by group,
// the placement rule (lib/place.ts) and the cleanup of traces.

const DAY = 24 * 60 * 60 * 1000
const A = 'mailbox-a'
const B = 'mailbox-b'

type T = TestConvex<typeof schema>
type Folder = 'INBOX' | 'Sent' | 'Archive' | 'Trash' | 'Later'
const ALL: Folder[] = ['INBOX', 'Sent', 'Archive', 'Trash', 'Later']

async function enable(t: T, mailboxKey = A) {
  await t.mutation(api.mailbox.enable, { mailboxKey, ownerKey: `owner-${mailboxKey}` })
  await t.mutation(api.mailbox.addFolders, { mailboxKey, folders: ['Archive', 'Trash', 'Later'] })
}

async function add(
  t: T,
  messageKey: string,
  overrides: Partial<{
    mailboxKey: string
    folder: Folder
    seen: boolean
    flags: string[]
    groupKey: string
    kind: 'in' | 'out'
  }> = {},
) {
  return await t.mutation(api.entries.add, {
    mailboxKey: A,
    folder: 'INBOX',
    messageKey,
    size: 100,
    internalDate: 1_700_000_000_000,
    seen: false,
    groupKey: 'g1',
    kind: 'in',
    ...overrides,
  })
}

async function folders(t: T, mailboxKey = A) {
  const rows = await t.query(api.folders.list, { mailboxKey })
  return Object.fromEntries(rows.map((row) => [row.folder, row])) as Record<
    Folder,
    (typeof rows)[number]
  >
}

async function seq(t: T, mailboxKey = A) {
  return (await t.query(api.mailbox.get, { mailboxKey }))?.seq
}

async function allRows(t: T) {
  return await t.run(async (ctx) => await ctx.db.query('entries').collect())
}

async function live(t: T, folder: Folder, mailboxKey = A) {
  const page = await t.query(api.entries.list, { mailboxKey, folder, limit: 1000 })
  return page.rows
}

async function moveGroup(
  t: T,
  targets: { in?: Folder; out?: Folder },
  options: { groupKey?: string; limit?: number; mailboxKey?: string } = {},
) {
  return await t.mutation(api.entries.moveGroup, {
    mailboxKey: options.mailboxKey ?? A,
    groupKey: options.groupKey ?? 'g1',
    targets,
    limit: options.limit ?? 1000,
  })
}

describe('mailBridge: folders and moving', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-09T10:00:00Z'))
    t = convexTest(schema, mailBridgeModules)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('enable creates only INBOX and Sent; addFolders adds, without touching what exists', async () => {
    expect(
      await t.mutation(api.mailbox.addFolders, { mailboxKey: A, folders: ['Trash'] }),
    ).toBeNull()
    await t.mutation(api.mailbox.enable, { mailboxKey: A, ownerKey: 'o' })
    expect(Object.keys(await folders(t))).toEqual(['INBOX', 'Sent'])
    await add(t, 'm1')
    const inbox = (await folders(t)).INBOX

    vi.setSystemTime(new Date('2026-10-09T11:00:00Z'))
    expect(
      await t.mutation(api.mailbox.addFolders, { mailboxKey: A, folders: ['Archive', 'INBOX'] }),
    ).toEqual(['Archive'])
    expect(
      await t.mutation(api.mailbox.addFolders, { mailboxKey: A, folders: ['Archive'] }),
    ).toEqual([])
    const after = await folders(t)
    expect(Object.keys(after)).toEqual(['INBOX', 'Sent', 'Archive'])
    expect(after.INBOX).toEqual(inbox)
    expect(after.Archive).toMatchObject({ uidNext: 1, exists: 0, unseen: 0 })
    expect(after.Archive.uidValidity).not.toBe(inbox.uidValidity)
  })

  it('folder change: new UID in the target, tombstone move in the old folder, flags and counters move along', async () => {
    await enable(t)
    await add(t, 'm1', { flags: ['\\Flagged'] })
    await add(t, 'm2', { seen: true })
    const before = await seq(t)

    const result = await moveGroup(t, { in: 'Archive' })
    expect(result.isDone).toBe(true)
    expect(result.moved).toEqual([
      { messageKey: 'm1', from: 'INBOX', fromUid: 1, fromLive: true, to: 'Archive', uid: 1 },
      { messageKey: 'm2', from: 'INBOX', fromUid: 2, fromLive: true, to: 'Archive', uid: 2 },
    ])
    expect(await seq(t)).toBe((before ?? 0) + 1)
    const state = await folders(t)
    expect(state.INBOX).toMatchObject({ exists: 0, unseen: 0, uidNext: 3 })
    expect(state.Archive).toMatchObject({ exists: 2, unseen: 1, uidNext: 3 })
    expect((await live(t, 'Archive')).map((row) => [row.messageKey, row.flags])).toEqual([
      ['m1', ['\\Flagged']],
      ['m2', ['\\Seen']],
    ])

    // A mirror of the old folder learns of the removal as an ordinary change.
    const changes = await t.query(api.entries.changes, {
      mailboxKey: A,
      folder: 'INBOX',
      sinceModseq: 0,
      limit: 100,
    })
    expect(changes.rows.map((row) => [row.uid, row.live, row.removedBy])).toEqual([
      [1, false, 'move'],
      [2, false, 'move'],
    ])

    // Once more: nothing to do, nothing written.
    const again = await moveGroup(t, { in: 'Archive' })
    expect(again).toEqual({ moved: [], isDone: true })
    expect(await seq(t)).toBe((before ?? 0) + 1)
  })

  it('a UID never comes back to life: back into the old folder means a new UID', async () => {
    await enable(t)
    await add(t, 'm1')
    await moveGroup(t, { in: 'Archive' })
    const back = await moveGroup(t, { in: 'INBOX' })
    expect(back.moved).toEqual([
      { messageKey: 'm1', from: 'Archive', fromUid: 1, fromLive: true, to: 'INBOX', uid: 2 },
    ])
    expect((await live(t, 'INBOX')).map((row) => row.uid)).toEqual([2])
    expect(await t.query(api.entries.byUid, { mailboxKey: A, folder: 'INBOX', uid: 1 })).toBeNull()
    expect((await allRows(t)).filter((row) => row.live)).toHaveLength(1)
  })

  it('one target per kind: incoming ones move, sent ones stay; a missing kind stays where it is', async () => {
    await enable(t)
    await add(t, 'in1')
    await add(t, 'out1', { folder: 'Sent', kind: 'out', seen: true })
    await add(t, 'fremd', { groupKey: 'g2' })

    await moveGroup(t, { in: 'Archive', out: 'Sent' })
    expect((await live(t, 'Archive')).map((row) => row.messageKey)).toEqual(['in1'])
    expect((await live(t, 'Sent')).map((row) => row.messageKey)).toEqual(['out1'])
    expect((await live(t, 'INBOX')).map((row) => row.messageKey)).toEqual(['fremd'])

    await moveGroup(t, { in: 'Trash', out: 'Trash' })
    expect((await live(t, 'Trash')).map((row) => row.messageKey).sort()).toEqual(['in1', 'out1'])

    await moveGroup(t, { out: 'Sent' })
    expect((await live(t, 'Trash')).map((row) => row.messageKey)).toEqual(['in1'])
    expect((await live(t, 'Sent')).map((row) => row.uid)).toEqual([2])
  })

  it('works in batches: isDone false until everything has moved', async () => {
    await enable(t)
    for (let index = 0; index < 7; index += 1) await add(t, `m${index}`)
    const first = await moveGroup(t, { in: 'Archive' }, { limit: 3 })
    expect(first.moved).toHaveLength(3)
    expect(first.isDone).toBe(false)
    const second = await moveGroup(t, { in: 'Archive' }, { limit: 3 })
    expect(second.moved).toHaveLength(3)
    expect(second.isDone).toBe(false)
    const third = await moveGroup(t, { in: 'Archive' }, { limit: 3 })
    expect(third.moved).toHaveLength(1)
    expect(third.isDone).toBe(true)
    expect((await folders(t)).Archive.exists).toBe(7)
  })

  it('hidden stays hidden while the message is in the same folder; in the next folder it reappears', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2')
    await moveGroup(t, { in: 'Trash' })
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'Trash', uids: [1] })
    expect((await live(t, 'Trash')).map((row) => row.messageKey)).toEqual(['m2'])

    // The same rule once more (Trash): m1 does not come back.
    const before = await seq(t)
    expect((await moveGroup(t, { in: 'Trash' })).moved).toEqual([])
    expect(await add(t, 'm1', { folder: 'Trash' })).toEqual({
      uid: 1,
      created: false,
      moved: false,
    })
    expect(await seq(t)).toBe(before)

    // The group comes back into the inbox: both messages, the hidden one too.
    const back = await moveGroup(t, { in: 'INBOX' })
    expect(back.moved.map((row) => `${row.messageKey} ${row.fromLive}`).sort()).toEqual([
      'm1 false',
      'm2 true',
    ])
    expect((await live(t, 'INBOX')).map((row) => row.messageKey).sort()).toEqual(['m1', 'm2'])
    expect((await folders(t)).Trash).toMatchObject({ exists: 0, unseen: 0 })
    // The earlier tombstone is only a trace now.
    const rows = await allRows(t)
    expect(rows.filter((row) => row.removedBy === 'client')).toHaveLength(0)
    expect(
      rows.filter((row) => row.messageKey === 'm1' && row.removedBy === undefined),
    ).toHaveLength(1)
  })

  it('add into another folder is a change: live, hidden, permanently deleted', async () => {
    await enable(t)
    await add(t, 'lebt', { flags: ['$Wichtig'] })
    await add(t, 'versteckt')
    await add(t, 'weg')
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [2] })
    await t.mutation(api.entries.remove, { messageKey: 'weg' })

    // Live elsewhere: new UID in the target, the row's values move along, not the call's.
    expect(await add(t, 'lebt', { folder: 'Archive', seen: true })).toEqual({
      uid: 1,
      created: false,
      moved: true,
    })
    expect(await live(t, 'Archive')).toMatchObject([{ messageKey: 'lebt', flags: ['$Wichtig'] }])
    // Hidden elsewhere: appears in the target, the old tombstone becomes a trace.
    expect(await add(t, 'versteckt', { folder: 'Archive' })).toEqual({
      uid: 2,
      created: false,
      moved: true,
    })
    // Permanently deleted: stays gone, whatever the folder.
    const before = await seq(t)
    expect(await add(t, 'weg', { folder: 'Archive' })).toEqual({
      uid: 3,
      created: false,
      moved: false,
    })
    expect(await seq(t)).toBe(before)
    expect((await live(t, 'Archive')).map((row) => row.messageKey)).toEqual(['lebt', 'versteckt'])
    expect((await folders(t)).INBOX).toMatchObject({ exists: 0, unseen: 0 })
    expect((await allRows(t)).filter((row) => row.removedBy === 'client')).toHaveLength(0)
  })

  it('a trace\'s retention period starts with the change, even if the message was hidden for long', async () => {
    await enable(t)
    await add(t, 'm1')
    const hidden = Date.now()
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [1] })

    vi.setSystemTime(new Date(hidden + 40 * DAY))
    await moveGroup(t, { in: 'Archive' })
    // Cleanup with a 30-day retention period: the trace is from today and stays.
    expect(
      await t.mutation(api.maintenance.cleanup, { before: Date.now() - 30 * DAY, limit: 100 }),
    ).toEqual({ deleted: 0, isDone: true })
    const trace = (await allRows(t)).find((row) => row.removedBy === 'move')
    expect(trace).toMatchObject({ folder: 'INBOX', uid: 1, removedAt: hidden + 40 * DAY })
    // The removal state in the old folder is that of the hiding.
    expect(trace?.removedModseq).toBe(trace?.modseq)
  })

  it('moveGroup reads only what is not in the target: if there is nothing to do, an entry in the target does not interfere', async () => {
    await enable(t)
    for (let index = 0; index < 30; index += 1) await add(t, `m${index}`, { folder: 'Archive' })
    await add(t, 'spät')
    // 30 rows are already in place; a batch of two is enough for the one that is due.
    expect(await moveGroup(t, { in: 'Archive' }, { limit: 2 })).toMatchObject({
      moved: [{ messageKey: 'spät', to: 'Archive', uid: 31 }],
      isDone: true,
    })
    // Rows without a kind belong to no target.
    await t.mutation(api.entries.add, {
      mailboxKey: A,
      folder: 'INBOX',
      messageKey: 'ohne-art',
      size: 1,
      internalDate: 1,
      seen: false,
      groupKey: 'g1',
    })
    expect(await moveGroup(t, { in: 'Archive', out: 'Archive' })).toEqual({
      moved: [],
      isDone: true,
    })
  })

  it('resolve leads from an old UID to the live row, but not to hidden or deleted ones', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2')
    await add(t, 'm3')
    await moveGroup(t, { in: 'Archive' })
    expect(
      await t.query(api.entries.resolve, { mailboxKey: A, folder: 'INBOX', uid: 1 }),
    ).toMatchObject({ messageKey: 'm1', folder: 'Archive', uid: 1, moved: true })
    expect(
      await t.query(api.entries.resolve, { mailboxKey: A, folder: 'Archive', uid: 1 }),
    ).toMatchObject({ messageKey: 'm1', folder: 'Archive', moved: false })

    // Several changes including a return: every earlier UID leads to today's row.
    await add(t, 'm4', { groupKey: 'g4' })
    await moveGroup(t, { in: 'Trash' }, { groupKey: 'g4' })
    await moveGroup(t, { in: 'INBOX' }, { groupKey: 'g4' })
    for (const [folder, uid] of [
      ['INBOX', 4],
      ['Trash', 1],
    ] as const) {
      expect(await t.query(api.entries.resolve, { mailboxKey: A, folder, uid })).toMatchObject({
        messageKey: 'm4',
        folder: 'INBOX',
        uid: 5,
        moved: true,
      })
    }

    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'Archive', uids: [2] })
    await t.mutation(api.entries.remove, { messageKey: 'm3' })
    for (const uid of [2, 3]) {
      expect(await t.query(api.entries.resolve, { mailboxKey: A, folder: 'INBOX', uid })).toBeNull()
      expect(
        await t.query(api.entries.resolve, { mailboxKey: A, folder: 'Archive', uid }),
      ).toBeNull()
    }
    expect(
      await t.query(api.entries.resolve, { mailboxKey: A, folder: 'INBOX', uid: 99 }),
    ).toBeNull()
    // A foreign mailbox resolves nothing.
    await t.mutation(api.mailbox.enable, { mailboxKey: B, ownerKey: 'o' })
    expect(
      await t.query(api.entries.resolve, { mailboxKey: B, folder: 'INBOX', uid: 1 }),
    ).toBeNull()
  })

  it('remove after folder changes: the current row becomes purge, a permanently deleted message never comes back', async () => {
    await enable(t)
    await add(t, 'm1')
    await moveGroup(t, { in: 'Archive' })
    await moveGroup(t, { in: 'Trash' })
    expect(await t.mutation(api.entries.remove, { messageKey: 'm1' })).toBe(true)
    const rows = await allRows(t)
    expect(rows.map((row) => `${row.folder} ${row.removedBy}`).sort()).toEqual([
      'Archive move',
      'INBOX move',
      'Trash purge',
    ])
    expect((await folders(t)).Trash).toMatchObject({ exists: 0, unseen: 0 })

    const before = await seq(t)
    expect((await moveGroup(t, { in: 'INBOX' })).moved).toEqual([])
    expect(await add(t, 'm1')).toEqual({ uid: 1, created: false, moved: false })
    expect(await seq(t)).toBe(before)
    expect((await allRows(t)).filter((row) => row.live)).toHaveLength(0)
  })

  it('cleanup removes traces and purge after the retention period and records the state for mirrors', async () => {
    await enable(t)
    await add(t, 'm1')
    await add(t, 'm2')
    await moveGroup(t, { in: 'Archive' })
    const moved = Date.now()

    vi.setSystemTime(new Date(moved + 31 * DAY))
    expect(await t.mutation(api.maintenance.cleanup, { before: moved, limit: 100 })).toEqual({
      deleted: 0,
      isDone: true,
    })
    expect(await t.mutation(api.maintenance.cleanup, { before: moved + DAY, limit: 1 })).toEqual({
      deleted: 1,
      isDone: false,
    })
    expect(await t.mutation(api.maintenance.cleanup, { before: moved + DAY, limit: 100 })).toEqual({
      deleted: 1,
      isDone: true,
    })
    expect((await allRows(t)).map((row) => [row.folder, row.live])).toEqual([
      ['Archive', true],
      ['Archive', true],
    ])
    // A mirror from before the change can no longer learn of the removal.
    const stale = await t.query(api.entries.changes, {
      mailboxKey: A,
      folder: 'INBOX',
      sinceModseq: 1,
      limit: 100,
    })
    expect(stale.resync).toBe(true)
  })

  it('tagMany adds group and kind afterwards, without any mail client seeing anything', async () => {
    await t.mutation(api.mailbox.enable, { mailboxKey: A, ownerKey: 'o' })
    await t.mutation(api.entries.add, {
      mailboxKey: A,
      folder: 'INBOX',
      messageKey: 'alt',
      size: 1,
      internalDate: 1,
      seen: false,
    })
    await t.mutation(api.mailbox.addFolders, { mailboxKey: A, folders: ['Archive'] })
    // Without a group, moveGroup does not find the row.
    expect((await moveGroup(t, { in: 'Archive' })).moved).toEqual([])
    const before = { seq: await seq(t), modseq: (await folders(t)).INBOX.highestModseq }
    const rows = [{ messageKey: 'alt', groupKey: 'g1', kind: 'in' as const }]
    expect(await t.mutation(api.entries.tagMany, { mailboxKey: A, rows })).toBe(1)
    expect(await t.mutation(api.entries.tagMany, { mailboxKey: A, rows })).toBe(0)
    expect(await t.mutation(api.entries.tagMany, { mailboxKey: B, rows })).toBe(0)
    expect({ seq: await seq(t), modseq: (await folders(t)).INBOX.highestModseq }).toEqual(before)
    expect((await moveGroup(t, { in: 'Archive' })).moved).toHaveLength(1)
  })

  it('groups of two mailboxes do not touch', async () => {
    await enable(t, A)
    await enable(t, B)
    await add(t, 'a1')
    await add(t, 'b1', { mailboxKey: B })
    await moveGroup(t, { in: 'Archive' }, { mailboxKey: B })
    expect((await live(t, 'INBOX', A)).map((row) => row.messageKey)).toEqual(['a1'])
    expect((await live(t, 'Archive', B)).map((row) => row.messageKey)).toEqual(['b1'])
    // `add` with a foreign mailbox does not move an existing message.
    expect(await add(t, 'a1', { mailboxKey: B, folder: 'Trash' })).toEqual({
      uid: 1,
      created: false,
      moved: false,
    })
    expect((await live(t, 'INBOX', A)).map((row) => row.messageKey)).toEqual(['a1'])
    expect(await live(t, 'Trash', B)).toEqual([])
  })

  it('random run: counters, UIDs and "at most one current row" hold against a model', async () => {
    await enable(t)
    // Fixed seed (mulberry32 with Math.imul), so that a failure is repeatable.
    let state = 0x9e3779b9
    const random = () => {
      state = (state + 0x6d2b79f5) | 0
      let x = Math.imul(state ^ (state >>> 15), 1 | state)
      x = (x + Math.imul(x ^ (x >>> 7), 61 | x)) ^ x
      return ((x ^ (x >>> 14)) >>> 0) / 4294967296
    }
    const pick = <V>(values: readonly V[]) => values[Math.floor(random() * values.length)]

    type Model = {
      folder: Folder
      state: 'live' | 'hidden' | 'gone'
      seen: boolean
      group: string
      kind: 'in' | 'out'
    }
    const model = new Map<string, Model>()
    const usedUids = new Map<Folder, Set<number>>(ALL.map((name) => [name, new Set<number>()]))
    const groups = ['g1', 'g2', 'g3']
    let next = 0

    for (let step = 0; step < 400; step += 1) {
      const roll = random()
      if (roll < 0.25 || model.size === 0) {
        const key = `m${next++}`
        const entry: Model = {
          folder: pick(ALL),
          state: 'live',
          seen: random() < 0.5,
          group: pick(groups),
          kind: pick(['in', 'out'] as const),
        }
        await add(t, key, {
          folder: entry.folder,
          seen: entry.seen,
          groupKey: entry.group,
          kind: entry.kind,
        })
        model.set(key, entry)
      } else if (roll < 0.6) {
        const group = pick(groups)
        const targets = {
          ...(random() < 0.8 ? { in: pick(ALL) } : {}),
          ...(random() < 0.5 ? { out: pick(ALL) } : {}),
        }
        let done = false
        while (!done) done = (await moveGroup(t, targets, { groupKey: group, limit: 5 })).isDone
        for (const entry of model.values()) {
          const target = targets[entry.kind]
          if (entry.group !== group || !target || entry.state === 'gone') continue
          if (entry.folder === target) continue
          entry.folder = target
          entry.state = 'live'
        }
      } else if (roll < 0.68) {
        // `add` for an existing message, as the backfill or a second write path would do.
        const [key, entry] = pick([...model.entries()])
        const target = pick(ALL)
        await add(t, key, { folder: target, groupKey: entry.group, kind: entry.kind })
        if (entry.state !== 'gone' && entry.folder !== target) {
          entry.folder = target
          entry.state = 'live'
        }
      } else if (roll < 0.75) {
        const [key, entry] = pick([...model.entries()])
        if (entry.state !== 'live') continue
        const row = (await live(t, entry.folder)).find((item) => item.messageKey === key)
        if (!row) throw new Error(`Schritt ${step}: ${key} fehlt in ${entry.folder}`)
        await t.mutation(api.entries.hide, { mailboxKey: A, folder: entry.folder, uids: [row.uid] })
        entry.state = 'hidden'
      } else if (roll < 0.9) {
        const [key, entry] = pick([...model.entries()])
        const seen = random() < 0.5
        await t.mutation(api.entries.setSeenMany, {
          mailboxKey: A,
          changes: [{ messageKey: key, seen }],
        })
        if (entry.state === 'live') entry.seen = seen
      } else {
        const [key, entry] = pick([...model.entries()])
        await t.mutation(api.entries.remove, { messageKey: key })
        entry.state = 'gone'
      }

      if (step % 20 !== 19) continue
      const state = await folders(t)
      const rows = await allRows(t)
      for (const name of ALL) {
        const expected = [...model.entries()].filter(
          ([, entry]) => entry.state === 'live' && entry.folder === name,
        )
        const actual = await live(t, name)
        expect(actual.map((row) => row.messageKey).sort(), `Schritt ${step}, ${name}`).toEqual(
          expected.map(([key]) => key).sort(),
        )
        expect(state[name].exists, `exists ${name}`).toBe(expected.length)
        expect(state[name].unseen, `unseen ${name}`).toBe(
          expected.filter(([, entry]) => !entry.seen).length,
        )
        // UIDs rise strictly per folder and are never reused.
        const uids = rows.filter((row) => row.folder === name).map((row) => row.uid)
        expect(new Set(uids).size).toBe(uids.length)
        for (const uid of uids) usedUids.get(name)?.add(uid)
        expect(state[name].uidNext).toBeGreaterThan(Math.max(0, ...(usedUids.get(name) ?? [])))
      }
      for (const key of model.keys()) {
        const current = rows.filter((row) => row.messageKey === key && row.removedBy !== 'move')
        expect(current, `current rows of ${key}`).toHaveLength(1)
      }
    }
  })
})
