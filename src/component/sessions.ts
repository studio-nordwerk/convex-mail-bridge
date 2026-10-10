// Login and session tokens of the mail clients, with a throttle for failed attempts.
//
// The bridge logs in per connection with address and device password (through the app's edge)
// and gets a session token for exactly one mailbox. Only the SHA-256 of both is stored here. A
// token is bound to its device password: every check reads it along, a revocation takes effect
// immediately.
//
// A mailbox's data (folders, rows, counters) belongs to the owner who enabled it
// (`state.ownerKey`). Login and every token check require that device password AND data belong to
// the same owner; the app then compares that owner with the mailbox's current owner. If a mailbox
// is reassigned to another person, that person sees neither the counters nor the rows of the
// predecessor.
//
// `login` returns a result and does not throw: a thrown error would roll back the counted failed
// attempt, and the throttle would never count. Only invalid attempts are counted; a valid device
// password always logs in (`lib/access.ts`).

import { v } from 'convex/values'
import { mutation, query } from './_generated/server.js'
import type { MutationCtx, QueryCtx } from './_generated/server.js'
import type { Doc } from './_generated/dataModel.js'
import {
  IP_LIMIT,
  LAST_USED_STEP_MS,
  MAX_ATTEMPT_ROWS,
  PAIR_LIMIT,
  REVOKED_RETENTION_MS,
  SESSION_TTL_MS,
  SETUP_LINK_RETENTION_MS,
  THROTTLE_WINDOW_MS,
  afterFailure,
  isSha256Hex,
  isThrottleKey,
  retryAfterSeconds,
} from './lib/access.js'
import { findState, pageSize } from './lib/ledger.js'

/** The number of tokens `seqMany` checks per call. */
export const MAX_SEQ_SESSIONS = 200

async function findAttempts(ctx: QueryCtx, key: string) {
  return await ctx.db
    .query('loginAttempts')
    .withIndex('by_key', (q) => q.eq('key', key))
    .first()
}

/** Count one failed attempt: in the row's current window, otherwise a new one starts. */
async function countFailure(
  ctx: MutationCtx,
  key: string,
  row: Doc<'loginAttempts'> | null,
  now: number,
) {
  const next = afterFailure(row, now)
  if (row) await ctx.db.patch('loginAttempts', row._id, next)
  else await ctx.db.insert('loginAttempts', { key, ...next })
}

/**
 * An invalid attempt: count it for pair and IP, or reject it without a write if one of the
 * limits is reached or the throttle takes no new row (`MAX_ATTEMPT_ROWS`).
 */
async function refuse(ctx: MutationCtx, keys: { pairKey: string; ipKey: string }, now: number) {
  const pair = await findAttempts(ctx, keys.pairKey)
  const ip = await findAttempts(ctx, keys.ipKey)
  const wait = Math.max(
    retryAfterSeconds(pair, PAIR_LIMIT, now) ?? 0,
    retryAfterSeconds(ip, IP_LIMIT, now) ?? 0,
  )
  if (wait > 0) return { kind: 'throttled' as const, retryAfterSeconds: wait }

  const newRows = (pair ? 0 : 1) + (ip ? 0 : 1)
  if (newRows > 0) {
    const total = await ctx.db.query('loginAttemptTotals').first()
    if ((total?.rows ?? 0) + newRows > MAX_ATTEMPT_ROWS) {
      return { kind: 'throttled' as const, retryAfterSeconds: THROTTLE_WINDOW_MS / 1000 }
    }
    if (total) await ctx.db.patch('loginAttemptTotals', total._id, { rows: total.rows + newRows })
    else await ctx.db.insert('loginAttemptTotals', { rows: newRows })
  }
  await countFailure(ctx, keys.pairKey, pair, now)
  await countFailure(ctx, keys.ipKey, ip, now)
  return { kind: 'auth' as const }
}

/**
 * Log in with a device password. Order: password, backfill state; the throttle only for
 * invalid attempts.
 *
 * - `ok`: the session token is created (12 hours). A valid, non-revoked device password of this
 *   mailbox always logs in, whatever the counters say: the throttle is neither read nor written
 *   for it.
 * - `preparing`: the password is right, the backfill is still running. No token, no failed attempt.
 * - `auth`: no such password, revoked, different owner (of the device password or of the
 *   data), or the app knows no mailbox (`mailboxKey: null`). Counts as a failed attempt for
 *   pair and IP. The lookup happens even without a mailbox, so that known and unknown addresses
 *   cost the same effort.
 * - `throttled`: an invalid attempt while the pair (address and client IP) or the IP already has
 *   too many failed attempts in the window, or the throttle is full. Nothing is written.
 */
export const login = mutation({
  args: {
    /** `null`: the app knows no enabled, active mailbox for the address. */
    mailboxKey: v.union(v.string(), v.null()),
    /** Owner of the mailbox today; device password and data must belong to them. */
    ownerKey: v.union(v.string(), v.null()),
    secretHash: v.string(),
    /** SHA-256 of the session token the app generated and hands out on `ok`. */
    tokenHash: v.string(),
    /** Throttle keys (hashes from the app): pair of address and client IP, and the IP alone. */
    pairKey: v.string(),
    ipKey: v.string(),
    /**
     * `false`: check only. On `ok` no token is created and `lastUsedAt` stays (the app does not let
     * the mail client in, for a reason of its own). Failed attempts count as usual.
     */
    issue: v.optional(v.boolean()),
  },
  returns: v.union(
    v.object({ kind: v.literal('ok'), expiresAt: v.number() }),
    v.object({ kind: v.literal('auth') }),
    v.object({ kind: v.literal('preparing') }),
    v.object({ kind: v.literal('throttled'), retryAfterSeconds: v.number() }),
  ),
  handler: async (ctx, args) => {
    // Malformed input is a bug in the app, not a failed attempt: throwing is fine here.
    if (!isSha256Hex(args.secretHash) || !isSha256Hex(args.tokenHash)) {
      throw new Error('mailBridge: invalid hash')
    }
    if (!isThrottleKey(args.pairKey) || !isThrottleKey(args.ipKey) || args.pairKey === args.ipKey) {
      throw new Error('mailBridge: invalid throttle key')
    }
    const now = Date.now()
    const mailboxKey = args.mailboxKey
    const device = await ctx.db
      .query('devicePasswords')
      .withIndex('by_mailboxKey_and_secretHash', (q) =>
        q.eq('mailboxKey', mailboxKey ?? '').eq('secretHash', args.secretHash),
      )
      .first()
    const found =
      mailboxKey !== null &&
      device !== null &&
      device.revokedAt === undefined &&
      device.ownerKey === args.ownerKey
        ? await findState(ctx, mailboxKey)
        : null
    // The data belongs to whoever enabled it: for any other owner there is none.
    const state = found && found.ownerKey === args.ownerKey ? found : null
    if (mailboxKey === null || !device || !state) return await refuse(ctx, args, now)
    if (state.backfillStatus === 'running') return { kind: 'preparing' as const }

    const expiresAt = now + SESSION_TTL_MS
    if (args.issue === false) return { kind: 'ok' as const, expiresAt }
    await ctx.db.insert('sessions', {
      tokenHash: args.tokenHash,
      mailboxKey,
      devicePasswordId: device._id,
      createdAt: now,
      expiresAt,
      run: state.backfillRun,
    })
    if (device.lastUsedAt === undefined || now - device.lastUsedAt >= LAST_USED_STEP_MS) {
      await ctx.db.patch('devicePasswords', device._id, { lastUsedAt: now })
    }
    return { kind: 'ok' as const, expiresAt }
  },
})

type StateCache = Map<string, Doc<'state'> | null>

/**
 * The valid session token for a hash, with its device password and the mailbox's data, or
 * `null`. Valid also means: device password and data belong to the same owner. Never writes.
 * `states`: data already read (for many tokens of the same mailbox).
 */
async function resolveSession(
  ctx: QueryCtx,
  tokenHash: string,
  now: number,
  states: StateCache = new Map(),
) {
  if (!isSha256Hex(tokenHash)) return null
  const session = await ctx.db
    .query('sessions')
    .withIndex('by_tokenHash', (q) => q.eq('tokenHash', tokenHash))
    .first()
  if (!session || session.expiresAt <= now) return null
  const device = await ctx.db.get('devicePasswords', session.devicePasswordId)
  if (!device || device.revokedAt !== undefined || device.mailboxKey !== session.mailboxKey) {
    return null
  }
  let state = states.get(session.mailboxKey)
  if (state === undefined) {
    state = await findState(ctx, session.mailboxKey)
    states.set(session.mailboxKey, state)
  }
  if (!state || state.ownerKey !== device.ownerKey) return null
  // A backfill (enabling, switching to folders) permanently invalidates every token from before:
  // the folders change in large batches, the mail client should log in again and re-read
  // afterwards, and the app checks at login whether the bridge may still serve the mailbox.
  // While it runs, there is no new token (`preparing`).
  if (state.backfillStatus === 'running') return null
  // Tokens from before this rule carry no run number; they are valid as long as no backfill has
  // advanced the state since they were issued (they expire within twelve hours).
  const sameRun =
    session.run !== undefined
      ? session.run === state.backfillRun
      : session.createdAt >= state.backfillUpdatedAt
  if (!sameRun) return null
  return { session, device, state }
}

/**
 * Check a session token. `null`: unknown, expired, the device password is revoked or deleted,
 * or it does not belong to the owner of the data. `ownerKey` is the owner of the data
 * (whoever enabled the mailbox): the app compares it with the mailbox's current owner.
 * No write; the app passes `now` in.
 */
export const check = query({
  args: { tokenHash: v.string(), now: v.number() },
  returns: v.union(
    v.null(),
    v.object({
      mailboxKey: v.string(),
      devicePasswordId: v.string(),
      ownerKey: v.string(),
      /**
       * The person behind the device password: the owner of the data or, for a shared
       * mailbox, a member. Whether they may still use the mailbox today is checked by the app.
       */
      userKey: v.string(),
      expiresAt: v.number(),
    }),
  ),
  handler: async (ctx, args) => {
    const found = await resolveSession(ctx, args.tokenHash, args.now)
    if (!found) return null
    return {
      mailboxKey: found.session.mailboxKey,
      devicePasswordId: found.device._id,
      ownerKey: found.state.ownerKey,
      userKey: found.device.userKey ?? found.state.ownerKey,
      expiresAt: found.session.expiresAt,
    }
  },
})

/**
 * Change counters of the mailboxes for many tokens, in request order (for the bridge's
 * wait for news). `null` per invalid token. A query, not a write.
 */
export const seqMany = query({
  args: { tokenHashes: v.array(v.string()), now: v.number() },
  returns: v.array(
    v.union(
      v.null(),
      v.object({
        mailboxKey: v.string(),
        ownerKey: v.string(),
        userKey: v.string(),
        seq: v.number(),
      }),
    ),
  ),
  handler: async (ctx, args) => {
    if (args.tokenHashes.length > MAX_SEQ_SESSIONS) throw new Error('mailBridge: too many sessions')
    const states: StateCache = new Map()
    const results = []
    for (const tokenHash of args.tokenHashes) {
      const found = await resolveSession(ctx, tokenHash, args.now, states)
      results.push(
        found
          ? {
              mailboxKey: found.session.mailboxKey,
              ownerKey: found.state.ownerKey,
              userKey: found.device.userKey ?? found.state.ownerKey,
              seq: found.state.seq,
            }
          : null,
      )
    }
    return results
  },
})

/**
 * Clean up, paginated: expired session tokens, expired throttle windows (with their cap),
 * device passwords revoked for longer than `REVOKED_RETENTION_MS`, and setup links expired for
 * longer than `SETUP_LINK_RETENTION_MS` (redeemed ones too: the device stays). `isDone:
 * false` = call again.
 */
export const cleanup = mutation({
  args: { now: v.number(), limit: v.number() },
  returns: v.object({
    sessions: v.number(),
    attempts: v.number(),
    devices: v.number(),
    setupLinks: v.number(),
    isDone: v.boolean(),
  }),
  handler: async (ctx, args) => {
    const limit = pageSize(args.limit)
    const sessions = await ctx.db
      .query('sessions')
      .withIndex('by_expiresAt', (q) => q.lte('expiresAt', args.now))
      .take(limit + 1)
    for (const row of sessions.slice(0, limit)) await ctx.db.delete('sessions', row._id)

    const attempts = await ctx.db
      .query('loginAttempts')
      .withIndex('by_windowStart', (q) => q.lte('windowStart', args.now - THROTTLE_WINDOW_MS))
      .take(limit + 1)
    const expired = attempts.slice(0, limit)
    for (const row of expired) await ctx.db.delete('loginAttempts', row._id)
    // Update the throttle's cap. If nothing expired is left, recount: that keeps the number
    // correct even if it ever drifted.
    const total = await ctx.db.query('loginAttemptTotals').first()
    if (total) {
      const rows =
        attempts.length <= limit
          ? (await ctx.db.query('loginAttempts').take(MAX_ATTEMPT_ROWS + 1)).length
          : Math.max(0, total.rows - expired.length)
      if (rows !== total.rows) await ctx.db.patch('loginAttemptTotals', total._id, { rows })
    }

    // `gt(0)`: rows without `revokedAt` (active passwords) come before all numbers in the index.
    const devices = await ctx.db
      .query('devicePasswords')
      .withIndex('by_revokedAt', (q) =>
        q.gt('revokedAt', 0).lte('revokedAt', args.now - REVOKED_RETENTION_MS),
      )
      .take(limit + 1)
    for (const row of devices.slice(0, limit)) await ctx.db.delete('devicePasswords', row._id)

    const setupLinks = await ctx.db
      .query('setupLinks')
      .withIndex('by_expiresAt', (q) => q.lte('expiresAt', args.now - SETUP_LINK_RETENTION_MS))
      .take(limit + 1)
    for (const row of setupLinks.slice(0, limit)) await ctx.db.delete('setupLinks', row._id)

    return {
      sessions: Math.min(sessions.length, limit),
      attempts: Math.min(attempts.length, limit),
      devices: Math.min(devices.length, limit),
      setupLinks: Math.min(setupLinks.length, limit),
      isDone:
        sessions.length <= limit &&
        attempts.length <= limit &&
        devices.length <= limit &&
        setupLinks.length <= limit,
    }
  },
})
