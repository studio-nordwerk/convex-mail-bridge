import { convexTest, type TestConvex } from 'convex-test'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import schema from './schema.js'
import { api } from './_generated/api.js'
import { mailBridgeModules } from './test.modules.js'
import {
  MAX_DEVICE_PASSWORDS,
  SETUP_LINK_RETENTION_MS,
  SETUP_LINK_TTL_MS,
  SETUP_TICKET_TTL_MS,
} from './lib/access.js'

// The component alone: setup links and download tickets (`setup.ts`). Each step checks state,
// expiry and owner itself, even if the app has already checked beforehand.
// Made-up keys; secrets only as hashes.

const A = 'mailbox-a'
const OWNER = 'owner-a'

type T = TestConvex<typeof schema>

const hash = (n: number) => n.toString(16).padStart(64, '0')

describe('component mailBridge › set-up by QR code', () => {
  let t: T

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-07T10:00:00Z'))
    t = convexTest(schema, mailBridgeModules)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  async function enable() {
    await t.mutation(api.mailbox.enable, { mailboxKey: A, ownerKey: OWNER })
    await t.mutation(api.mailbox.setBackfill, { mailboxKey: A, status: 'done' })
  }

  async function create(token: number, label = 'iPhone') {
    const result = await t.mutation(api.setup.create, {
      mailboxKey: A,
      ownerKey: OWNER,
      label,
      tokenHash: hash(token),
    })
    if (result.kind !== 'created') throw new Error(`no link: ${result.kind}`)
    return result
  }

  const claim = (token: number, ticket: number, ownerKey = OWNER) =>
    t.mutation(api.setup.claim, { tokenHash: hash(token), ticketHash: hash(ticket), ownerKey })
  const redeem = (ticket: number, secret: number, ownerKey = OWNER) =>
    t.mutation(api.setup.redeem, { ticketHash: hash(ticket), secretHash: hash(secret), ownerKey })
  const devices = () => t.query(api.devices.list, { mailboxKey: A })

  it('not enabled or someone else\'s data: no link', async () => {
    const args = { mailboxKey: A, ownerKey: OWNER, label: 'iPhone', tokenHash: hash(1) }
    expect(await t.mutation(api.setup.create, args)).toEqual({ kind: 'disabled' })
    await enable()
    expect(await t.mutation(api.setup.create, { ...args, ownerKey: 'someone' })).toEqual({
      kind: 'owner',
    })
    await expect(t.mutation(api.setup.create, { ...args, tokenHash: 'not-a-hash' })).rejects.toThrow(
      'invalid hash',
    )
    await expect(t.mutation(api.setup.create, { ...args, label: ' ' })).rejects.toThrow('label')
  })

  it('claim checks state, expiry and owner itself', async () => {
    await enable()
    await create(1)
    // Different owner: nothing.
    expect(await claim(1, 100, 'someone')).toEqual({ kind: 'gone' })
    // Once ...
    expect(await claim(1, 101)).toMatchObject({
      kind: 'claimed',
      expiresAt: Date.now() + SETUP_TICKET_TTL_MS,
    })
    // ... and not twice.
    expect(await claim(1, 102)).toEqual({ kind: 'gone' })
    // Expired.
    await create(2)
    vi.advanceTimersByTime(SETUP_LINK_TTL_MS)
    expect(await claim(2, 103)).toEqual({ kind: 'gone' })
    // Unknown.
    expect(await claim(3, 104)).toEqual({ kind: 'gone' })
  })

  it('redeem checks state, expiry and owner itself and creates at most one device', async () => {
    await enable()
    await create(1, "Mara's iPhone")
    // Without redeeming the link there is no ticket.
    expect(await redeem(101, 900)).toEqual({ kind: 'gone' })
    await claim(1, 101)
    expect(await redeem(101, 900, 'someone')).toEqual({ kind: 'gone' })
    const created = await redeem(101, 901)
    expect(created).toMatchObject({ kind: 'created', label: "Mara's iPhone" })
    expect(await redeem(101, 902)).toEqual({ kind: 'gone' })
    expect((await devices()).map((row) => row.label)).toEqual(["Mara's iPhone"])

    // Ticket expired.
    await create(2)
    await claim(2, 102)
    vi.advanceTimersByTime(SETUP_TICKET_TTL_MS)
    expect(await redeem(102, 903)).toEqual({ kind: 'gone' })
    expect(await devices()).toHaveLength(1)
  })

  it('device limit: no link, no redeeming, no device; the link stays open', async () => {
    await enable()
    await create(1)
    await claim(1, 101)
    for (let n = 0; n < MAX_DEVICE_PASSWORDS; n++) {
      await t.mutation(api.devices.create, {
        mailboxKey: A,
        ownerKey: OWNER,
        label: `Device ${n}`,
        secretHash: hash(500 + n),
      })
    }
    expect(await redeem(101, 900)).toEqual({ kind: 'limit' })
    expect(
      await t.mutation(api.setup.create, {
        mailboxKey: A,
        ownerKey: OWNER,
        label: 'iPad',
        tokenHash: hash(2),
      }),
    ).toEqual({ kind: 'devices' })
    expect(await devices()).toHaveLength(MAX_DEVICE_PASSWORDS)
  })

  it('revokeAll (mailbox disabled) invalidates open links and tickets', async () => {
    await enable()
    await create(1)
    await create(2)
    await claim(2, 102)
    // Another mailbox stays as it is.
    await t.mutation(api.mailbox.enable, { mailboxKey: 'mailbox-b', ownerKey: 'b' })
    await t.mutation(api.setup.create, {
      mailboxKey: 'mailbox-b',
      ownerKey: 'b',
      label: 'iPhone',
      tokenHash: hash(3),
    })
    expect(await t.mutation(api.devices.revokeAll, { mailboxKey: A })).toEqual({
      devices: 0,
      sessions: 0,
      setupLinks: 2,
    })
    expect(await claim(1, 101)).toEqual({ kind: 'gone' })
    expect(await redeem(102, 900)).toEqual({ kind: 'gone' })
    expect(await t.mutation(api.devices.revokeAll, { mailboxKey: A })).toMatchObject({
      setupLinks: 0,
    })
    expect(await claim(3, 103, 'b')).toMatchObject({ kind: 'claimed' })
  })

  it('discard and cleanup', async () => {
    await enable()
    const open = await create(1)
    expect(await t.mutation(api.setup.discard, { id: open.id, mailboxKey: 'other' })).toBe(false)
    // Open ones only: a redeemed link stays, an open one does not.
    await create(2)
    const { id: claimedId } = await claim(2, 102).then((result) => {
      if (result.kind !== 'claimed') throw new Error(result.kind)
      return result
    })
    expect(
      await t.mutation(api.setup.discard, { id: claimedId, mailboxKey: A, onlyOpen: true }),
    ).toBe(false)
    expect(await redeem(102, 900)).toMatchObject({ kind: 'created' })
    expect(await t.query(api.setup.get, { id: claimedId })).toMatchObject({
      state: 'downloaded',
      deviceId: expect.any(String),
    })
    expect(
      await t.mutation(api.setup.discard, { id: open.id, mailboxKey: A, onlyOpen: true }),
    ).toBe(true)
    expect(await claim(1, 101)).toEqual({ kind: 'gone' })
    expect(await t.query(api.setup.get, { id: open.id })).toMatchObject({
      state: 'open',
      expiresAt: Date.now(),
    })
    vi.advanceTimersByTime(SETUP_LINK_RETENTION_MS - 1)
    expect(
      (await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 100 })).setupLinks,
    ).toBe(0)
    vi.advanceTimersByTime(1)
    expect(
      (await t.mutation(api.sessions.cleanup, { now: Date.now(), limit: 100 })).setupLinks,
    ).toBe(1)
    expect(await t.query(api.setup.get, { id: open.id })).toBeNull()
  })
})
