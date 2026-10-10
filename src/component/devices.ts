// Device passwords: one password per mail client, for exactly one mailbox.
//
// Only the SHA-256 of the password arrives here; the app generates the password itself and shows it
// once. The app checks who may create and revoke (the mailbox owner and, for shared mailboxes,
// every member for their OWN devices). A revocation takes effect immediately: every session token
// check reads the device password too (`sessions.check`).
//
// Shared mailboxes: `userKey` on a device password names the member it belongs to; if absent, it
// belongs to the owner (`ownerKey`, as before shared mailboxes existed). `ownerKey` stays on every
// row as the owner of the data. Lists, limit and revocation apply per person.

import { v } from 'convex/values'
import { mutation, query } from './_generated/server.js'
import type { MutationCtx, QueryCtx } from './_generated/server.js'
import type { Id } from './_generated/dataModel.js'
import { MAX_DEVICE_PASSWORDS, cleanLabel, isSha256Hex } from './lib/access.js'
import { findState } from './lib/ledger.js'
import { expireSetupLinks, expireUserSetupLinks } from './lib/setupLinks.js'

/** Session tokens one call deletes at most. What remains is no longer valid and expires. */
const SESSION_DROP_LIMIT = 500
const REVOKE_ALL_SESSION_BUDGET = 2000

/** Active device passwords that disabling a mailbox reads at most (all people). */
const REVOKE_ALL_DEVICES = 500

/**
 * `userKey` as it appears on the rows: the owner's devices carry none (even if the app names the
 * owner's key), so old and new rows of the owner are the same.
 */
export function memberKey(ownerKey: string, userKey: string | undefined): string | undefined {
  return userKey === undefined || userKey === ownerKey ? undefined : userKey
}

/**
 * Active device passwords of ONE person on a mailbox (at most `MAX_DEVICE_PASSWORDS`, one more
 * read). `userKey: undefined` = the owner's.
 */
export async function activeDevices(ctx: QueryCtx, mailboxKey: string, userKey?: string) {
  return await ctx.db
    .query('devicePasswords')
    .withIndex('by_mailboxKey_and_userKey_and_revokedAt', (q) =>
      q.eq('mailboxKey', mailboxKey).eq('userKey', userKey).eq('revokedAt', undefined),
    )
    .take(MAX_DEVICE_PASSWORDS + 1)
}

/** Active device passwords of a mailbox, of all people (for disabling). */
async function allActiveDevices(ctx: QueryCtx, mailboxKey: string) {
  return await ctx.db
    .query('devicePasswords')
    .withIndex('by_mailboxKey_and_revokedAt', (q) =>
      q.eq('mailboxKey', mailboxKey).eq('revokedAt', undefined),
    )
    .take(REVOKE_ALL_DEVICES)
}

/** Delete the session tokens of a device password; returns how many. */
async function dropSessions(ctx: MutationCtx, deviceId: Id<'devicePasswords'>, limit: number) {
  const sessions = await ctx.db
    .query('sessions')
    .withIndex('by_devicePasswordId', (q) => q.eq('devicePasswordId', deviceId))
    .take(limit)
  for (const session of sessions) await ctx.db.delete('sessions', session._id)
  return sessions.length
}

/**
 * Create a device password, for `create` and for setup by QR code (`setup.redeem`, in the same
 * transaction as redeeming the ticket). `disabled`: the mailbox is not enabled. `owner`: the
 * mailbox data belongs to a different owner than the one to create for (the mailbox was
 * transferred); there are no devices for that owner. `limit`: this person already has
 * `MAX_DEVICE_PASSWORDS` active ones on this mailbox (revoked ones do not count). `userKey`: the
 * member the device should belong to (absent = owner).
 */
export async function insertDevicePassword(
  ctx: MutationCtx,
  args: {
    mailboxKey: string
    ownerKey: string
    userKey?: string
    label: string
    secretHash: string
  },
) {
  const label = cleanLabel(args.label)
  if (!label) throw new Error('mailBridge: invalid label')
  if (!isSha256Hex(args.secretHash)) throw new Error('mailBridge: invalid hash')
  const state = await findState(ctx, args.mailboxKey)
  if (!state) return { kind: 'disabled' as const }
  if (state.ownerKey !== args.ownerKey) return { kind: 'owner' as const }
  const userKey = memberKey(args.ownerKey, args.userKey)
  const active = await activeDevices(ctx, args.mailboxKey, userKey)
  if (active.length >= MAX_DEVICE_PASSWORDS) return { kind: 'limit' as const }
  // A password exists only once per mailbox (a revoked one does not come back to life that way).
  const taken = await ctx.db
    .query('devicePasswords')
    .withIndex('by_mailboxKey_and_secretHash', (q) =>
      q.eq('mailboxKey', args.mailboxKey).eq('secretHash', args.secretHash),
    )
    .first()
  if (taken) throw new Error('mailBridge: secret already used')
  const createdAt = Date.now()
  const id = await ctx.db.insert('devicePasswords', {
    mailboxKey: args.mailboxKey,
    ownerKey: args.ownerKey,
    ...(userKey !== undefined ? { userKey } : {}),
    label,
    secretHash: args.secretHash,
    createdAt,
  })
  return { kind: 'created' as const, id, label, createdAt }
}

/** Create a device password (`insertDevicePassword`). */
export const create = mutation({
  args: {
    mailboxKey: v.string(),
    ownerKey: v.string(),
    userKey: v.optional(v.string()),
    label: v.string(),
    secretHash: v.string(),
  },
  returns: v.union(
    v.object({ kind: v.literal('created'), id: v.string(), createdAt: v.number() }),
    v.object({ kind: v.literal('limit') }),
    v.object({ kind: v.literal('disabled') }),
    v.object({ kind: v.literal('owner') }),
  ),
  handler: async (ctx, args) => {
    const result = await insertDevicePassword(ctx, args)
    if (result.kind !== 'created') return result
    return { kind: 'created' as const, id: result.id, createdAt: result.createdAt }
  },
})

/**
 * Active device passwords of a person on a mailbox, oldest first (`userKey` absent = the owner's).
 * Never with the hash, never those of another person.
 */
export const list = query({
  args: { mailboxKey: v.string(), userKey: v.optional(v.string()) },
  returns: v.array(
    v.object({
      id: v.string(),
      label: v.string(),
      createdAt: v.number(),
      lastUsedAt: v.optional(v.number()),
    }),
  ),
  handler: async (ctx, args) => {
    const rows = await activeDevices(ctx, args.mailboxKey, args.userKey)
    return rows
      .slice(0, MAX_DEVICE_PASSWORDS)
      .sort((a, b) => a.createdAt - b.createdAt)
      .map((row) => ({
        id: row._id,
        label: row.label,
        createdAt: row.createdAt,
        ...(row.lastUsedAt !== undefined ? { lastUsedAt: row.lastUsedAt } : {}),
      }))
  },
})

/**
 * Whose is the active device password with this hash on this mailbox? `null`: there is none (or
 * it is revoked). `userKey: null`: the owner's. For sign-in: the app uses this to check whether
 * this person may still use the mailbox today, before a session token is created.
 */
export const holder = query({
  args: { mailboxKey: v.string(), secretHash: v.string() },
  returns: v.union(v.null(), v.object({ userKey: v.union(v.string(), v.null()) })),
  handler: async (ctx, args) => {
    const device = await ctx.db
      .query('devicePasswords')
      .withIndex('by_mailboxKey_and_secretHash', (q) =>
        q.eq('mailboxKey', args.mailboxKey).eq('secretHash', args.secretHash),
      )
      .first()
    if (!device || device.revokedAt !== undefined) return null
    return { userKey: device.userKey ?? null }
  },
})

/**
 * Revoke a device password; its session tokens are deleted in the same mutation. `false`:
 * unknown, already revoked, from a different mailbox or another person's device (`userKey` absent
 * = the owner's devices; all the same to the caller).
 */
export const revoke = mutation({
  args: { mailboxKey: v.string(), id: v.string(), userKey: v.optional(v.string()) },
  returns: v.boolean(),
  handler: async (ctx, args) => {
    const id = ctx.db.normalizeId('devicePasswords', args.id)
    const device = id ? await ctx.db.get('devicePasswords', id) : null
    if (
      !device ||
      device.mailboxKey !== args.mailboxKey ||
      device.revokedAt !== undefined ||
      device.userKey !== args.userKey
    ) {
      return false
    }
    await ctx.db.patch('devicePasswords', device._id, { revokedAt: Date.now() })
    await dropSessions(ctx, device._id, SESSION_DROP_LIMIT)
    return true
  },
})

/**
 * Revoke all device passwords of a mailbox and delete their session tokens (the mailbox is being
 * disabled). Afterwards no session token of this mailbox is valid any more, not even one this
 * call's deletion did not reach: its device password is revoked. All setup links that could still
 * become a device are voided as well: re-enabling the mailbox within their remaining time would
 * otherwise bring an old QR image back to life.
 */
export const revokeAll = mutation({
  args: { mailboxKey: v.string() },
  returns: v.object({ devices: v.number(), sessions: v.number(), setupLinks: v.number() }),
  handler: async (ctx, args) => {
    const devices = await allActiveDevices(ctx, args.mailboxKey)
    const now = Date.now()
    let sessions = 0
    for (const device of devices) {
      await ctx.db.patch('devicePasswords', device._id, { revokedAt: now })
      const budget = Math.min(SESSION_DROP_LIMIT, REVOKE_ALL_SESSION_BUDGET - sessions)
      if (budget > 0) sessions += await dropSessions(ctx, device._id, budget)
    }
    const setupLinks = await expireSetupLinks(ctx, args.mailboxKey, now)
    return { devices: devices.length, sessions, setupLinks }
  },
})

/**
 * A member loses access to a shared mailbox: revoke THEIR device passwords on this mailbox, delete
 * their session tokens and void their setup links. The devices of the owner and the other members
 * stay. A session token this deletion does not reach is no longer valid either: its device
 * password is revoked.
 */
export const revokeUser = mutation({
  args: { mailboxKey: v.string(), userKey: v.string() },
  returns: v.object({ devices: v.number(), sessions: v.number(), setupLinks: v.number() }),
  handler: async (ctx, args) => {
    const devices = await activeDevices(ctx, args.mailboxKey, args.userKey)
    const now = Date.now()
    let sessions = 0
    for (const device of devices) {
      await ctx.db.patch('devicePasswords', device._id, { revokedAt: now })
      sessions += await dropSessions(ctx, device._id, SESSION_DROP_LIMIT)
    }
    const setupLinks = await expireUserSetupLinks(ctx, args.mailboxKey, args.userKey, now)
    return { devices: devices.length, sessions, setupLinks }
  },
})
