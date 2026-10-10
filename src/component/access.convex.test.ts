import { convexTest, type TestConvex } from 'convex-test'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import schema from './schema.js'
import { api } from './_generated/api.js'
import { mailBridgeModules } from './test.modules.js'
import {
  IP_LIMIT,
  MAX_ATTEMPT_ROWS,
  MAX_DEVICE_PASSWORDS,
  PAIR_LIMIT,
  REVOKED_RETENTION_MS,
  SESSION_TTL_MS,
  THROTTLE_WINDOW_MS,
} from './lib/access.js'

// The component alone: device passwords, sign-in, session tokens, throttle, cleanup, plus the
// additions on the rows for the edge (`byUid`, `resync`). Invented keys, no addresses.

const MINUTE = 60 * 1000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR
const A = 'mailbox-a'
const B = 'mailbox-b'
const OWNER_A = `owner-${A}`

type T = TestConvex<typeof schema>

/** A SHA-256 in hex for a number: the component sees secrets only this way. */
const hash = (n: number) => n.toString(16).padStart(64, '0')
const SECRET = hash(1)
const WRONG = hash(2)

async function enable(t: T, mailboxKey = A, done = true) {
  await t.mutation(api.mailbox.enable, { mailboxKey, ownerKey: `owner-${mailboxKey}` })
  if (done) await t.mutation(api.mailbox.setBackfill, { mailboxKey, status: 'done' })
}

async function createDevice(t: T, secretHash = SECRET, mailboxKey = A, label = 'iPhone') {
  const result = await t.mutation(api.devices.create, {
    mailboxKey,
    ownerKey: `owner-${mailboxKey}`,
    label,
    secretHash,
  })
  if (result.kind !== 'created') throw new Error(`no device password: ${result.kind}`)
  return result.id
}

let tokens = 1000

/** Sign-in as from the edge: hashes and throttle keys arrive ready-made. */
async function login(
  t: T,
  overrides: Partial<{
    mailboxKey: string | null
    ownerKey: string | null
    secretHash: string
    tokenHash: string
    pairKey: string
    ipKey: string
  }> = {},
) {
  tokens += 1
  const args = {
    mailboxKey: A as string | null,
    ownerKey: OWNER_A as string | null,
    secretHash: SECRET,
    tokenHash: hash(tokens),
    pairKey: 'p:mac',
    ipKey: 'i:mac',
    ...overrides,
  }
  return { result: await t.mutation(api.sessions.login, args), tokenHash: args.tokenHash }
}

async function check(t: T, tokenHash: string) {
  return await t.query(api.sessions.check, { tokenHash, now: Date.now() })
}

async function rows<
  Table extends 'devicePasswords' | 'sessions' | 'loginAttempts' | 'loginAttemptTotals',
>(t: T, table: Table) {
  return await t.run(async (ctx) => await ctx.db.query(table).collect())
}

async function attempts(t: T, key: string) {
  return (await rows(t, 'loginAttempts')).find((row) => row.key === key)?.count ?? 0
}

/** Everything the throttle holds: for comparing "did an attempt write anything?". */
async function throttleState(t: T) {
  return JSON.stringify([await rows(t, 'loginAttempts'), await rows(t, 'loginAttemptTotals')])
}

describe('mailBridge: device passwords, sign-in, session tokens (component alone)', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-07T10:00:00Z'))
    t = convexTest(schema, mailBridgeModules)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  // ─── Device passwords ─────────────────────────────────────────────────

  it('create only for an enabled mailbox; the list never names the hash', async () => {
    expect(
      await t.mutation(api.devices.create, {
        mailboxKey: A,
        ownerKey: OWNER_A,
        label: 'iPhone',
        secretHash: SECRET,
      }),
    ).toEqual({ kind: 'disabled' })
    await enable(t)
    const id = await createDevice(t, SECRET, A, '  Mac  in Büro ')
    const list = await t.query(api.devices.list, { mailboxKey: A })
    expect(list).toEqual([{ id, label: 'Mac in Büro', createdAt: Date.now() }])
    expect(JSON.stringify(list)).not.toContain(SECRET)
    // Other mailbox: nothing.
    expect(await t.query(api.devices.list, { mailboxKey: B })).toEqual([])
    // An empty or too long label and a hash of the wrong shape are caller errors.
    for (const bad of [
      { label: '   ', secretHash: hash(5) },
      { label: 'x'.repeat(41), secretHash: hash(5) },
      { label: 'iPad', secretHash: 'no-hash' },
    ]) {
      await expect(
        t.mutation(api.devices.create, { mailboxKey: A, ownerKey: OWNER_A, ...bad }),
      ).rejects.toThrow()
    }
  })

  it('at most ten active per mailbox; revoked ones do not count', async () => {
    await enable(t)
    await enable(t, B)
    const ids: string[] = []
    for (let n = 0; n < MAX_DEVICE_PASSWORDS; n++) {
      ids.push(await createDevice(t, hash(100 + n), A, `Device ${n}`))
    }
    const eleventh = {
      mailboxKey: A,
      ownerKey: OWNER_A,
      label: 'too many',
      secretHash: hash(200),
    }
    expect(await t.mutation(api.devices.create, eleventh)).toEqual({ kind: 'limit' })
    // The limit applies per mailbox.
    await createDevice(t, hash(300), B)

    expect(await t.mutation(api.devices.revoke, { mailboxKey: A, id: ids[0] })).toBe(true)
    expect(await t.mutation(api.devices.create, eleventh)).toMatchObject({ kind: 'created' })
    expect(await t.query(api.devices.list, { mailboxKey: A })).toHaveLength(MAX_DEVICE_PASSWORDS)
    expect(await t.mutation(api.devices.create, { ...eleventh, secretHash: hash(201) })).toEqual({
      kind: 'limit',
    })
  })

  it('revocation deletes the session tokens of that device in the same mutation, only in its own mailbox', async () => {
    await enable(t)
    await enable(t, B)
    const phone = await createDevice(t, hash(11), A, 'iPhone')
    await createDevice(t, hash(12), A, 'Mac')
    const one = await login(t, { secretHash: hash(11) })
    const two = await login(t, { secretHash: hash(11) })
    const mac = await login(t, { secretHash: hash(12) })
    expect(one.result.kind).toBe('ok')
    expect(await rows(t, 'sessions')).toHaveLength(3)

    // From another mailbox, unknown, not an id: the same "does not exist", nothing changed.
    expect(await t.mutation(api.devices.revoke, { mailboxKey: B, id: phone })).toBe(false)
    expect(await t.mutation(api.devices.revoke, { mailboxKey: A, id: 'no-id' })).toBe(false)
    expect(await check(t, one.tokenHash)).not.toBeNull()

    expect(await t.mutation(api.devices.revoke, { mailboxKey: A, id: phone })).toBe(true)
    expect(await check(t, one.tokenHash)).toBeNull()
    expect(await check(t, two.tokenHash)).toBeNull()
    expect(await check(t, mac.tokenHash)).toMatchObject({ mailboxKey: A })
    const left = await rows(t, 'sessions')
    expect(left.map((row) => row.tokenHash)).toEqual([mac.tokenHash])
    // Already revoked: not a second time, and the password no longer signs in.
    expect(await t.mutation(api.devices.revoke, { mailboxKey: A, id: phone })).toBe(false)
    expect((await login(t, { secretHash: hash(11) })).result).toEqual({ kind: 'auth' })
    expect((await t.query(api.devices.list, { mailboxKey: A })).map((row) => row.label)).toEqual([
      'Mac',
    ])
  })

  it('revokeAll invalidates all device passwords and session tokens of a mailbox', async () => {
    await enable(t)
    await enable(t, B)
    await createDevice(t, hash(11), A, 'iPhone')
    await createDevice(t, hash(12), A, 'Mac')
    await createDevice(t, hash(13), B, 'iPad')
    const phone = await login(t, { secretHash: hash(11) })
    const mac = await login(t, { secretHash: hash(12) })
    const other = await login(t, { mailboxKey: B, ownerKey: `owner-${B}`, secretHash: hash(13) })

    expect(await t.mutation(api.devices.revokeAll, { mailboxKey: A })).toEqual({
      devices: 2,
      sessions: 2,
      setupLinks: 0,
    })
    expect(await check(t, phone.tokenHash)).toBeNull()
    expect(await check(t, mac.tokenHash)).toBeNull()
    expect(await t.query(api.devices.list, { mailboxKey: A })).toEqual([])
    expect((await login(t, { secretHash: hash(12) })).result).toEqual({ kind: 'auth' })
    // The other mailbox stays as it is.
    expect(await check(t, other.tokenHash)).toMatchObject({ mailboxKey: B })
    expect(await t.query(api.devices.list, { mailboxKey: B })).toHaveLength(1)
    // A second call finds nothing.
    expect(await t.mutation(api.devices.revokeAll, { mailboxKey: A })).toEqual({
      devices: 0,
      sessions: 0,
      setupLinks: 0,
    })
  })

  // ─── Signing in ───────────────────────────────────────────────────────

  it('correct password: session token for twelve hours, bound to mailbox and device password', async () => {
    await enable(t)
    const id = await createDevice(t)
    const { result, tokenHash } = await login(t)
    expect(result).toEqual({ kind: 'ok', expiresAt: Date.now() + SESSION_TTL_MS })
    expect(await check(t, tokenHash)).toEqual({
      mailboxKey: A,
      devicePasswordId: id,
      ownerKey: OWNER_A,
      userKey: OWNER_A,
      expiresAt: Date.now() + SESSION_TTL_MS,
    })
    // What is stored is the hash of the session token that the app passed in.
    expect((await rows(t, 'sessions')).map((row) => row.tokenHash)).toEqual([tokenHash])
    expect(await check(t, hash(999_999))).toBeNull()
    expect(await check(t, 'no-hash')).toBeNull()
  })

  it('wrong, revoked, other mailbox, different owner, no mailbox: always auth', async () => {
    await enable(t)
    await enable(t, B)
    const id = await createDevice(t)
    await createDevice(t, hash(13), B)

    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    // The password of another mailbox does not open this one.
    expect((await login(t, { secretHash: hash(13) })).result).toEqual({ kind: 'auth' })
    // The mailbox has meanwhile got a different owner: the old password no longer counts.
    expect((await login(t, { ownerKey: 'owner-new' })).result).toEqual({ kind: 'auth' })
    // The app knows no mailbox for the address (unknown, disabled, not enabled).
    expect((await login(t, { mailboxKey: null, ownerKey: null })).result).toEqual({ kind: 'auth' })
    expect(await rows(t, 'sessions')).toEqual([])

    await t.mutation(api.devices.revoke, { mailboxKey: A, id })
    expect((await login(t)).result).toEqual({ kind: 'auth' })
    expect(await rows(t, 'sessions')).toEqual([])
  })

  it('the data belongs to whoever enabled it: for a different owner there is no device, no sign-in and no session token', async () => {
    await enable(t)
    await createDevice(t)
    const own = await login(t)
    expect(own.result.kind).toBe('ok')
    // The mailbox now belongs to a different person. A device for them is not created.
    expect(
      await t.mutation(api.devices.create, {
        mailboxKey: A,
        ownerKey: 'owner-new',
        label: 'Mac',
        secretHash: hash(21),
      }),
    ).toEqual({ kind: 'owner' })
    expect(await rows(t, 'devicePasswords')).toHaveLength(1)

    // Even if there were a device of the new owner (planted by hand here, with a session token):
    // the data belongs to the predecessor, so no sign-in and no valid session token.
    const planted = await t.run(async (ctx) => {
      const deviceId = await ctx.db.insert('devicePasswords', {
        mailboxKey: A,
        ownerKey: 'owner-new',
        label: 'planted',
        secretHash: hash(22),
        createdAt: Date.now(),
      })
      await ctx.db.insert('sessions', {
        tokenHash: hash(23),
        mailboxKey: A,
        devicePasswordId: deviceId,
        createdAt: Date.now(),
        expiresAt: Date.now() + SESSION_TTL_MS,
      })
      return deviceId
    })
    expect(planted).toBeDefined()
    expect((await login(t, { ownerKey: 'owner-new', secretHash: hash(22) })).result).toEqual({
      kind: 'auth',
    })
    expect(await check(t, hash(23))).toBeNull()
    expect(
      await t.query(api.sessions.seqMany, {
        tokenHashes: [hash(23), own.tokenHash],
        now: Date.now(),
      }),
    ).toEqual([null, { mailboxKey: A, ownerKey: OWNER_A, userKey: OWNER_A, seq: 0 }])
    // The predecessor's session token stays valid in the component; the app checks it against the
    // mailbox's current owner today (`ownerKey` is the owner of the data).
    expect(await check(t, own.tokenHash)).toMatchObject({ ownerKey: OWNER_A })

    // Conversely: a (planted) device of another person in the owner's data. That does not sign in
    // either, although data and mailbox fit together.
    await t.run(async (ctx) => {
      await ctx.db.insert('devicePasswords', {
        mailboxKey: A,
        ownerKey: 'owner-old',
        label: 'foreign',
        secretHash: hash(24),
        createdAt: Date.now(),
      })
    })
    expect((await login(t, { secretHash: hash(24) })).result).toEqual({ kind: 'auth' })
  })

  it('while the backfill is still running, a correct password gets "preparing" and no session token', async () => {
    await enable(t, A, false)
    await createDevice(t)
    expect((await login(t)).result).toEqual({ kind: 'preparing' })
    expect(await rows(t, 'sessions')).toEqual([])
    // No failed attempt; a wrong password is still a failed attempt now.
    expect(await rows(t, 'loginAttempts')).toEqual([])
    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    expect(await attempts(t, 'p:mac')).toBe(1)

    await t.mutation(api.mailbox.setBackfill, { mailboxKey: A, status: 'done' })
    expect((await login(t)).result.kind).toBe('ok')
  })

  it('lastUsedAt is written at most once per hour', async () => {
    await enable(t)
    await createDevice(t)
    const start = Date.now()
    const lastUsed = async () => (await t.query(api.devices.list, { mailboxKey: A }))[0].lastUsedAt
    expect(await lastUsed()).toBeUndefined()
    await login(t)
    expect(await lastUsed()).toBe(start)
    vi.setSystemTime(start + 59 * MINUTE)
    await login(t)
    expect(await lastUsed()).toBe(start)
    vi.setSystemTime(start + 60 * MINUTE)
    await login(t)
    expect(await lastUsed()).toBe(start + 60 * MINUTE)
  })

  it('malformed hashes and keys are caller errors, not failed attempts', async () => {
    await enable(t)
    await createDevice(t)
    await expect(login(t, { secretHash: 'plaintext' })).rejects.toThrow()
    await expect(login(t, { tokenHash: 'plaintext' })).rejects.toThrow()
    await expect(login(t, { pairKey: 'x'.repeat(129) })).rejects.toThrow()
    await expect(login(t, { ipKey: '' })).rejects.toThrow()
    await expect(login(t, { pairKey: 'same', ipKey: 'same' })).rejects.toThrow()
    expect(await rows(t, 'loginAttempts')).toEqual([])
  })

  // ─── Throttle ─────────────────────────────────────────────────────────

  it('result instead of throw: the counted failed attempt stays stored, even without a mailbox', async () => {
    const unknown = await login(t, { mailboxKey: null, ownerKey: null })
    expect(unknown.result).toEqual({ kind: 'auth' })
    expect(await rows(t, 'loginAttempts')).toMatchObject([
      { key: 'p:mac', windowStart: Date.now(), count: 1 },
      { key: 'i:mac', windowStart: Date.now(), count: 1 },
    ])
    expect(await rows(t, 'loginAttemptTotals')).toMatchObject([{ rows: 2 }])
    await login(t, { mailboxKey: null, ownerKey: null })
    expect(await attempts(t, 'p:mac')).toBe(2)
    expect(await attempts(t, 'i:mac')).toBe(2)
    // No hash of a wrong password in the throttle rows.
    expect(await throttleState(t)).not.toContain(SECRET)
  })

  it('per pair: after ten failed attempts a further one is rejected without a write until the window ends', async () => {
    await enable(t)
    await createDevice(t)
    const start = Date.now()
    for (let n = 0; n < PAIR_LIMIT; n++) {
      vi.setSystemTime(start + n * 1000)
      expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    }
    vi.setSystemTime(start + MINUTE)
    const full = await throttleState(t)
    expect((await login(t, { secretHash: WRONG })).result).toEqual({
      kind: 'throttled',
      retryAfterSeconds: (THROTTLE_WINDOW_MS - MINUTE) / 1000,
    })
    expect((await login(t, { secretHash: hash(77) })).result.kind).toBe('throttled')
    // Rejected means: nothing counted, nothing written.
    expect(await throttleState(t)).toBe(full)
    expect(await attempts(t, 'p:mac')).toBe(PAIR_LIMIT)
    expect(await attempts(t, 'i:mac')).toBe(PAIR_LIMIT)

    vi.setSystemTime(start + THROTTLE_WINDOW_MS - 1)
    expect((await login(t, { secretHash: WRONG })).result).toEqual({
      kind: 'throttled',
      retryAfterSeconds: 1,
    })
    // The window counts from the first failed attempt: after it, checking and counting resume.
    vi.setSystemTime(start + THROTTLE_WINDOW_MS)
    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    expect(await attempts(t, 'p:mac')).toBe(1)
  })

  it('a valid device password always signs in: 50 failed attempts with an old password do not lock out the second device behind the same IP', async () => {
    await enable(t)
    const phone = await createDevice(t, hash(11), A, 'iPhone')
    await createDevice(t, hash(12), A, 'Mac')
    await t.mutation(api.devices.revoke, { mailboxKey: A, id: phone })
    // The iPhone repeats its revoked password from the same Wi-Fi as the Mac: the same pair of
    // address and IP, the same IP.
    const outcomes: string[] = []
    for (let n = 0; n < 50; n++)
      outcomes.push((await login(t, { secretHash: hash(11) })).result.kind)
    expect(outcomes.slice(0, PAIR_LIMIT)).toEqual(Array.from({ length: PAIR_LIMIT }, () => 'auth'))
    expect(new Set(outcomes.slice(PAIR_LIMIT))).toEqual(new Set(['throttled']))
    expect(await attempts(t, 'p:mac')).toBe(PAIR_LIMIT)
    expect(await rows(t, 'loginAttempts')).toHaveLength(2)

    // The Mac signs in regardless, now and again; it does not touch the throttle doing so.
    const before = await throttleState(t)
    const mac = await login(t, { secretHash: hash(12) })
    expect(mac.result.kind).toBe('ok')
    expect(await check(t, mac.tokenHash)).toMatchObject({ mailboxKey: A })
    expect((await login(t, { secretHash: hash(11) })).result.kind).toBe('throttled')
    expect((await login(t, { secretHash: hash(12) })).result.kind).toBe('ok')
    expect(await throttleState(t)).toBe(before)
  })

  it('per IP: thirty failed attempts across many addresses lock further invalid ones from this IP, valid ones never', async () => {
    await enable(t)
    await createDevice(t)
    const start = Date.now()
    for (let n = 0; n < IP_LIMIT; n++) {
      const tried = await login(t, {
        mailboxKey: null,
        ownerKey: null,
        pairKey: `p:rate-${n}`,
        ipKey: 'i:attacker',
      })
      expect(tried.result).toEqual({ kind: 'auth' })
    }
    // Each pair has only one failed attempt; rejection is because of the IP, a new pair too.
    const full = await throttleState(t)
    expect(
      (await login(t, { secretHash: WRONG, pairKey: 'p:new', ipKey: 'i:attacker' })).result,
    ).toEqual({ kind: 'throttled', retryAfterSeconds: THROTTLE_WINDOW_MS / 1000 })
    // Once rejected, no further rows appear.
    expect(await throttleState(t)).toBe(full)
    expect(await rows(t, 'loginAttempts')).toHaveLength(IP_LIMIT + 1)
    // The valid password signs in from this IP too; a different IP counts for itself.
    expect((await login(t, { pairKey: 'p:new', ipKey: 'i:attacker' })).result.kind).toBe('ok')
    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })

    vi.setSystemTime(start + THROTTLE_WINDOW_MS)
    expect(
      (await login(t, { secretHash: WRONG, pairKey: 'p:new', ipKey: 'i:attacker' })).result,
    ).toEqual({ kind: 'auth' })
  })

  it('a successful sign-in neither reads nor writes the throttle', async () => {
    await enable(t)
    await createDevice(t)
    for (let n = 0; n < PAIR_LIMIT - 1; n++) await login(t, { secretHash: WRONG })
    const before = await throttleState(t)
    expect((await login(t)).result.kind).toBe('ok')
    expect(await throttleState(t)).toBe(before)
    // The next failed attempt is the tenth, the one after it is rejected.
    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    expect((await login(t, { secretHash: WRONG })).result.kind).toBe('throttled')
    expect((await login(t)).result.kind).toBe('ok')
  })

  it('cap: when the throttle is full, no more rows are created; invalid attempts are rejected, valid ones sign in', async () => {
    await enable(t)
    await createDevice(t)
    const start = Date.now()
    // A known pair with a running window, then the table filled up to the cap.
    await login(t, { secretHash: WRONG })
    await t.run(async (ctx) => {
      for (let n = 2; n < MAX_ATTEMPT_ROWS; n++) {
        await ctx.db.insert('loginAttempts', { key: `x:${n}`, windowStart: start, count: 1 })
      }
      const [total] = await ctx.db.query('loginAttemptTotals').collect()
      await ctx.db.patch('loginAttemptTotals', total._id, { rows: MAX_ATTEMPT_ROWS })
    })
    expect(await rows(t, 'loginAttempts')).toHaveLength(MAX_ATTEMPT_ROWS)
    const full = await throttleState(t)

    // A new pair or a new IP would need a new row: rejected, nothing written.
    for (const keys of [
      { pairKey: 'p:new', ipKey: 'i:new' },
      { pairKey: 'p:new', ipKey: 'i:mac' },
      { pairKey: 'p:mac', ipKey: 'i:new' },
    ]) {
      expect((await login(t, { secretHash: WRONG, ...keys })).result).toEqual({
        kind: 'throttled',
        retryAfterSeconds: THROTTLE_WINDOW_MS / 1000,
      })
    }
    expect(await throttleState(t)).toBe(full)
    // A valid password signs in, also from a new pair and a new IP.
    expect((await login(t, { pairKey: 'p:new', ipKey: 'i:new' })).result.kind).toBe('ok')
    expect(await throttleState(t)).toBe(full)
    // A known pair on a known IP needs no new row and keeps counting.
    expect((await login(t, { secretHash: WRONG })).result).toEqual({ kind: 'auth' })
    expect(await attempts(t, 'p:mac')).toBe(2)
    expect(await rows(t, 'loginAttempts')).toHaveLength(MAX_ATTEMPT_ROWS)

    // Cleanup makes room and recounts: afterwards counting resumes.
    vi.setSystemTime(start + THROTTLE_WINDOW_MS)
    for (;;) {
      const done = await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 1000 })
      if (done.isDone) break
    }
    expect(await rows(t, 'loginAttempts')).toEqual([])
    expect(await rows(t, 'loginAttemptTotals')).toMatchObject([{ rows: 0 }])
    expect(
      (await login(t, { secretHash: WRONG, pairKey: 'p:new', ipKey: 'i:new' })).result,
    ).toEqual({ kind: 'auth' })
    expect(await rows(t, 'loginAttemptTotals')).toMatchObject([{ rows: 2 }])
  })

  it('cleanup recounts the cap, even if the number has once drifted', async () => {
    await enable(t)
    await login(t, { mailboxKey: null, ownerKey: null })
    await t.run(async (ctx) => {
      const [total] = await ctx.db.query('loginAttemptTotals').collect()
      await ctx.db.patch('loginAttemptTotals', total._id, { rows: 999_999 })
    })
    expect((await login(t, { secretHash: WRONG, pairKey: 'p:x', ipKey: 'i:x' })).result.kind).toBe(
      'throttled',
    )
    // Nothing has expired, but the run recounts: two rows.
    await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 100 })
    expect(await rows(t, 'loginAttemptTotals')).toMatchObject([{ rows: 2 }])
    expect((await login(t, { secretHash: WRONG, pairKey: 'p:x', ipKey: 'i:x' })).result).toEqual({
      kind: 'auth',
    })
  })

  // ─── Session tokens ───────────────────────────────────────────────────

  it('a session token expires after twelve hours; an expired one is not a failed attempt', async () => {
    await enable(t)
    await createDevice(t)
    const start = Date.now()
    const { tokenHash } = await login(t)
    vi.setSystemTime(start + SESSION_TTL_MS - 1)
    expect(await check(t, tokenHash)).not.toBeNull()
    vi.setSystemTime(start + SESSION_TTL_MS)
    expect(await check(t, tokenHash)).toBeNull()
    expect(await rows(t, 'loginAttempts')).toEqual([])
    // Checking writes nothing: the row stays until cleanup comes.
    expect(await rows(t, 'sessions')).toHaveLength(1)
  })

  it('every check reads the device password too: if revoked or deleted, the session token is invalid', async () => {
    await enable(t)
    const id = await createDevice(t)
    const { tokenHash } = await login(t)
    const [device] = await rows(t, 'devicePasswords')
    expect(device._id).toBe(id)
    // Revocation without deleting the session tokens (as if the per-call limit had been reached).
    await t.run(async (ctx) => {
      await ctx.db.patch('devicePasswords', device._id, { revokedAt: Date.now() })
    })
    expect(await rows(t, 'sessions')).toHaveLength(1)
    expect(await check(t, tokenHash)).toBeNull()
    expect(
      await t.query(api.sessions.seqMany, { tokenHashes: [tokenHash], now: Date.now() }),
    ).toEqual([null])
    await t.run(async (ctx) => {
      await ctx.db.delete('devicePasswords', device._id)
    })
    expect(await check(t, tokenHash)).toBeNull()
  })

  it('seqMany: valid and invalid session tokens mixed, in the order of the request', async () => {
    await enable(t)
    await enable(t, B)
    await createDevice(t, hash(11), A)
    await createDevice(t, hash(13), B)
    const a = await login(t, { secretHash: hash(11) })
    const b = await login(t, { mailboxKey: B, ownerKey: `owner-${B}`, secretHash: hash(13) })
    const expired = await login(t, { secretHash: hash(11) })
    await t.run(async (ctx) => {
      const row = await ctx.db
        .query('sessions')
        .withIndex('by_tokenHash', (q) => q.eq('tokenHash', expired.tokenHash))
        .unique()
      if (row) await ctx.db.patch('sessions', row._id, { expiresAt: Date.now() })
    })
    // Something changes in mailbox A: its counter rises, B's does not.
    await t.mutation(api.entries.add, {
      mailboxKey: A,
      folder: 'INBOX',
      messageKey: 'm1',
      size: 10,
      internalDate: 1,
      seen: false,
    })
    const before = JSON.stringify(await rows(t, 'sessions'))
    const result = await t.query(api.sessions.seqMany, {
      tokenHashes: [b.tokenHash, 'junk-ü', a.tokenHash, expired.tokenHash, hash(77), a.tokenHash],
      now: Date.now(),
    })
    expect(result).toEqual([
      { mailboxKey: B, ownerKey: `owner-${B}`, userKey: `owner-${B}`, seq: 0 },
      null,
      { mailboxKey: A, ownerKey: OWNER_A, userKey: OWNER_A, seq: 1 },
      null,
      null,
      { mailboxKey: A, ownerKey: OWNER_A, userKey: OWNER_A, seq: 1 },
    ])
    expect(JSON.stringify(await rows(t, 'sessions'))).toBe(before)
    await expect(
      t.query(api.sessions.seqMany, {
        tokenHashes: Array.from({ length: 201 }, () => a.tokenHash),
        now: Date.now(),
      }),
    ).rejects.toThrow()
  })

  // ─── Cleanup ──────────────────────────────────────────────────────────

  it('cleanup deletes expired session tokens, old throttle windows and long-revoked passwords, paging', async () => {
    await enable(t)
    const start = Date.now()
    const old = await createDevice(t, hash(11), A, 'old')
    const recent = await createDevice(t, hash(12), A, 'newly revoked')
    await createDevice(t, hash(13), A, 'active')
    for (let n = 0; n < 5; n++) await login(t, { secretHash: hash(13) })
    for (let n = 0; n < 3; n++) {
      await login(t, { secretHash: WRONG, pairKey: `p:old-${n}`, ipKey: 'i:old' })
    }
    await t.mutation(api.devices.revoke, { mailboxKey: A, id: old })

    // Twelve hours later: the five session tokens and the four throttle rows are due.
    vi.setSystemTime(start + SESSION_TTL_MS)
    const fresh = await login(t, { secretHash: hash(13) })
    await login(t, { secretHash: WRONG, pairKey: 'p:fresh', ipKey: 'i:fresh' })
    const first = await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 2 })
    expect(first).toEqual({ sessions: 2, attempts: 2, devices: 0, setupLinks: 0, isDone: false })
    let turns = 1
    for (;;) {
      const next = await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 2 })
      turns += 1
      if (next.isDone) break
    }
    expect(turns).toBe(3)
    expect((await rows(t, 'sessions')).map((row) => row.tokenHash)).toEqual([fresh.tokenHash])
    expect((await rows(t, 'loginAttempts')).map((row) => row.key).sort()).toEqual([
      'i:fresh',
      'p:fresh',
    ])
    // Revoked passwords stay for 30 days; active ones always stay.
    expect(await rows(t, 'devicePasswords')).toHaveLength(3)

    vi.setSystemTime(start + REVOKED_RETENTION_MS - 1)
    await t.mutation(api.devices.revoke, { mailboxKey: A, id: recent })
    expect((await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 100 })).devices).toBe(
      0,
    )
    vi.setSystemTime(start + REVOKED_RETENTION_MS)
    expect(await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 100 })).toMatchObject({
      devices: 1,
      isDone: true,
    })
    expect((await rows(t, 'devicePasswords')).map((row) => row.label).sort()).toEqual([
      'active',
      'newly revoked',
    ])
    expect((await t.query(api.devices.list, { mailboxKey: A })).map((row) => row.label)).toEqual([
      'active',
    ])
  })
})

describe('mailBridge: rows for the edge (component alone)', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-07T10:00:00Z'))
    t = convexTest(schema, mailBridgeModules)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  async function add(messageKey: string, mailboxKey = A, folder: 'INBOX' | 'Sent' = 'INBOX') {
    return await t.mutation(api.entries.add, {
      mailboxKey,
      folder,
      messageKey,
      size: 100,
      internalDate: 1_700_000_000_000,
      seen: folder === 'Sent',
    })
  }

  async function changes(sinceModseq: number, mailboxKey = A, folder: 'INBOX' | 'Sent' = 'INBOX') {
    return await t.query(api.entries.changes, { mailboxKey, folder, sinceModseq, limit: 100 })
  }

  it('byUid finds only the live row in exactly this mailbox and folder', async () => {
    await enable(t)
    await enable(t, B)
    await add('a-1')
    await add('a-2')
    await add('a-sent', A, 'Sent')
    await add('b-1', B)
    expect(
      await t.query(api.entries.byUid, { mailboxKey: A, folder: 'INBOX', uid: 1 }),
    ).toMatchObject({
      uid: 1,
      messageKey: 'a-1',
    })
    // The same UID exists in every mailbox and folder: what counts is what is asked for.
    expect(
      await t.query(api.entries.byUid, { mailboxKey: B, folder: 'INBOX', uid: 1 }),
    ).toMatchObject({
      messageKey: 'b-1',
    })
    expect(
      await t.query(api.entries.byUid, { mailboxKey: A, folder: 'Sent', uid: 1 }),
    ).toMatchObject({
      messageKey: 'a-sent',
    })
    expect(await t.query(api.entries.byUid, { mailboxKey: B, folder: 'INBOX', uid: 2 })).toBeNull()
    expect(await t.query(api.entries.byUid, { mailboxKey: B, folder: 'Sent', uid: 1 })).toBeNull()
    // Removed: no live row any more.
    await t.mutation(api.entries.hide, { mailboxKey: A, folder: 'INBOX', uids: [2] })
    expect(await t.query(api.entries.byUid, { mailboxKey: A, folder: 'INBOX', uid: 2 })).toBeNull()
  })

  it('setFlags reports per row the change stamp and whether it changed', async () => {
    await enable(t)
    await add('m1')
    await add('m2')
    const result = await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1, 2],
      add: ['\\Flagged'],
      remove: [],
    })
    expect(result.map((row) => [row.uid, row.changed, row.modseq])).toEqual([
      [1, true, 4],
      [2, true, 5],
    ])
    const again = await t.mutation(api.entries.setFlags, {
      mailboxKey: A,
      folder: 'INBOX',
      uids: [1],
      add: ['\\Flagged'],
      remove: [],
    })
    expect(again).toEqual([
      {
        uid: 1,
        messageKey: 'm1',
        flags: ['\\Flagged'],
        modseq: 4,
        changed: false,
        seenChanged: false,
      },
    ])
  })

  it('resync: whoever never saw the removal of a since-deleted row must re-read', async () => {
    await enable(t)
    await enable(t, B)
    await add('m1') // modseq 2
    await add('m2') // 3
    await add('m3') // 4
    await add('b1', B)
    await add('s1', A, 'Sent')
    const start = Date.now()
    await t.mutation(api.entries.remove, { messageKey: 'm1' }) // 5
    vi.setSystemTime(start + DAY)
    await t.mutation(api.entries.remove, { messageKey: 'm3' }) // 6

    // As long as the removed rows still exist, every state learns of them: no resync.
    expect((await changes(0)).resync).toBe(false)
    expect((await changes(0)).rows.filter((row) => !row.live).map((row) => row.uid)).toEqual([1, 3])

    // Cleanup deletes the older of the two (up to and including change stamp 5).
    vi.setSystemTime(start + 31 * DAY)
    expect(await t.mutation(api.maintenance.cleanup, { before: start + HOUR, limit: 100 })).toEqual(
      { deleted: 1, isDone: true },
    )
    expect((await changes(4)).resync).toBe(true)
    expect((await changes(0)).resync).toBe(true)
    // Whoever already had state 5 has seen the removal and keeps reading changes.
    const current = await changes(5)
    expect(current.resync).toBe(false)
    expect(current.rows.map((row) => [row.uid, row.live])).toEqual([[3, false]])
    // Other folders and mailboxes are not affected.
    expect((await changes(0, A, 'Sent')).resync).toBe(false)
    expect((await changes(0, B)).resync).toBe(false)

    // The limit only rises: the next cleanup raises it to 6.
    expect(
      await t.mutation(api.maintenance.cleanup, { before: start + 2 * DAY, limit: 100 }),
    ).toEqual({ deleted: 1, isDone: true })
    expect((await changes(5)).resync).toBe(true)
    expect((await changes(6)).resync).toBe(false)
    const folders = await t.run(async (ctx) => await ctx.db.query('folders').collect())
    expect(
      folders
        .map((row) => `${row.mailboxKey} ${row.folder} ${row.resyncBelowModseq ?? '-'}`)
        .sort(),
    ).toEqual([`${A} INBOX 6`, `${A} Sent -`, `${B} INBOX -`, `${B} Sent -`])
    // For mail clients nothing changed through cleanup: counters and states stay.
    expect(await t.query(api.mailbox.get, { mailboxKey: A })).toMatchObject({ seq: 6 })
  })

  it('resync: a state above the highest of the folder cannot be carried forward', async () => {
    await enable(t)
    await add('m1') // modseq 2
    await add('m2') // 3
    const inbox = (await t.query(api.folders.list, { mailboxKey: A })).find(
      (row) => row.folder === 'INBOX',
    )
    expect(inbox?.highestModseq).toBe(3)
    // Exactly the highest state: nothing new, no resync.
    expect(await changes(3)).toEqual({ rows: [], isDone: true, resync: false })
    expect((await changes(2)).resync).toBe(false)
    expect((await changes(0)).resync).toBe(false)
    // Above it: this state never existed in this folder.
    expect(await changes(4)).toEqual({ rows: [], isDone: true, resync: true })
    expect((await changes(1_000_000)).resync).toBe(true)
    // Per folder: state 3 from INBOX is too high for the empty folder Sent (highest state 1).
    expect((await changes(1, A, 'Sent')).resync).toBe(false)
    expect((await changes(3, A, 'Sent')).resync).toBe(true)
    // As soon as the folder gets that far, the state counts again.
    await add('m3') // 4
    expect((await changes(4)).resync).toBe(false)
  })
})
