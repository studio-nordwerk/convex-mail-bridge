// Set-up by QR code: set-up links and download tickets.
//
// The owner creates a link in the app (256 bit, 10 minutes, once). The phone opens it; only the
// tap on "load profile" redeems it and trades it for a download ticket (256 bit, 60 seconds,
// once). Only redeeming the ticket creates the device password, in the same transaction that marks
// the link as done: one link gives at most one device, and a link that is never redeemed leaves
// none behind.
//
// Only the SHA-256 of link and ticket are stored here; the component never sees the password,
// only its hash. Who may create a link and whether the mailbox may still be used is checked by the
// app. The rules for devices apply here too: at most `MAX_DEVICE_PASSWORDS` active ones, and
// device and ledger belong to the same owner.

import { v } from 'convex/values'
import { mutation, query } from './_generated/server.js'
import type { MutationCtx, QueryCtx } from './_generated/server.js'
import {
  MAX_DEVICE_PASSWORDS,
  MAX_OPEN_SETUP_LINKS,
  SETUP_LINK_TTL_MS,
  SETUP_TICKET_TTL_MS,
  cleanLabel,
  isSha256Hex,
} from './lib/access.js'
import { findState } from './lib/ledger.js'
import { pendingUserSetupLinks } from './lib/setupLinks.js'
import { activeDevices, insertDevicePassword, memberKey } from './devices.js'
import { setupStateValidator } from './schema.js'

async function linkByToken(ctx: QueryCtx, tokenHash: string) {
  if (!isSha256Hex(tokenHash)) return null
  return await ctx.db
    .query('setupLinks')
    .withIndex('by_tokenHash', (q) => q.eq('tokenHash', tokenHash))
    .unique()
}

async function linkByTicket(ctx: QueryCtx, ticketHash: string) {
  if (!isSha256Hex(ticketHash)) return null
  return await ctx.db
    .query('setupLinks')
    .withIndex('by_ticketHash', (q) => q.eq('ticketHash', ticketHash))
    .unique()
}

/** Does the mailbox's data (still) belong to this owner? Otherwise no link comes to anything. */
async function ownStanding(ctx: QueryCtx, mailboxKey: string, ownerKey: string) {
  const state = await findState(ctx, mailboxKey)
  return state !== null && state.ownerKey === ownerKey
}

/**
 * Create a setup link. Only the SHA-256 of the link arrives here. `disabled`: not enabled.
 * `owner`: the data belongs to a different owner. `devices`: there are already
 * `MAX_DEVICE_PASSWORDS` active devices (otherwise loading the profile would only fail later).
 * `links`: there are already `MAX_OPEN_SETUP_LINKS` links that can still come to something.
 */
export const create = mutation({
  args: {
    mailboxKey: v.string(),
    ownerKey: v.string(),
    /** Shared mailboxes: the member creating the link (absent = owner). */
    userKey: v.optional(v.string()),
    label: v.string(),
    tokenHash: v.string(),
  },
  returns: v.union(
    v.object({ kind: v.literal('created'), id: v.string(), expiresAt: v.number() }),
    v.object({ kind: v.literal('disabled') }),
    v.object({ kind: v.literal('owner') }),
    v.object({ kind: v.literal('devices') }),
    v.object({ kind: v.literal('links') }),
  ),
  handler: async (ctx, args) => {
    const label = cleanLabel(args.label)
    if (!label) throw new Error('mailBridge: invalid label')
    if (!isSha256Hex(args.tokenHash)) throw new Error('mailBridge: invalid hash')
    const state = await findState(ctx, args.mailboxKey)
    if (!state) return { kind: 'disabled' as const }
    if (state.ownerKey !== args.ownerKey) return { kind: 'owner' as const }
    const now = Date.now()
    // Both limits apply per person (owner or this member), not per mailbox.
    const userKey = memberKey(args.ownerKey, args.userKey)
    if ((await activeDevices(ctx, args.mailboxKey, userKey)).length >= MAX_DEVICE_PASSWORDS) {
      return { kind: 'devices' as const }
    }
    const pending = await pendingUserSetupLinks(ctx, args.mailboxKey, userKey, now)
    if (pending.length >= MAX_OPEN_SETUP_LINKS) {
      return { kind: 'links' as const }
    }
    if (await linkByToken(ctx, args.tokenHash)) throw new Error('mailBridge: token already used')
    const expiresAt = now + SETUP_LINK_TTL_MS
    const id = await ctx.db.insert('setupLinks', {
      mailboxKey: args.mailboxKey,
      ownerKey: args.ownerKey,
      ...(userKey !== undefined ? { userKey } : {}),
      label,
      tokenHash: args.tokenHash,
      createdAt: now,
      expiresAt,
      state: 'open',
    })
    return { kind: 'created' as const, id, expiresAt }
  },
})

/**
 * An open, unexpired link for a hash, or `null`. Only reads, consumes nothing: for showing
 * the setup page and before redeeming (the app then checks the mailbox).
 */
export const peek = query({
  args: { tokenHash: v.string(), now: v.number() },
  returns: v.union(
    v.null(),
    v.object({
      id: v.string(),
      mailboxKey: v.string(),
      ownerKey: v.string(),
      /** The member who created the link; `null` = the owner. */
      userKey: v.union(v.string(), v.null()),
      label: v.string(),
      expiresAt: v.number(),
    }),
  ),
  handler: async (ctx, args) => {
    const link = await linkByToken(ctx, args.tokenHash)
    if (!link || link.state !== 'open' || link.expiresAt <= args.now) return null
    return {
      id: link._id,
      mailboxKey: link.mailboxKey,
      ownerKey: link.ownerKey,
      userKey: link.userKey ?? null,
      label: link.label,
      expiresAt: link.expiresAt,
    }
  },
})

/** A valid download ticket for a hash, or `null`. Only reads. */
export const peekTicket = query({
  args: { ticketHash: v.string(), now: v.number() },
  returns: v.union(
    v.null(),
    v.object({
      id: v.string(),
      mailboxKey: v.string(),
      ownerKey: v.string(),
      userKey: v.union(v.string(), v.null()),
      label: v.string(),
    }),
  ),
  handler: async (ctx, args) => {
    const link = await linkByTicket(ctx, args.ticketHash)
    if (!link || link.state !== 'claimed' || link.expiresAt <= args.now) return null
    return {
      id: link._id,
      mailboxKey: link.mailboxKey,
      ownerKey: link.ownerKey,
      userKey: link.userKey ?? null,
      label: link.label,
    }
  },
})

/** What became of the link, for the reply of `claim` and `redeem`. */
type Refusal = { kind: 'gone' } | { kind: 'limit' }

async function refuseUnusable(
  ctx: MutationCtx,
  link: { mailboxKey: string; ownerKey: string; userKey?: string },
  ownerKey: string,
): Promise<Refusal | null> {
  if (link.ownerKey !== ownerKey || !(await ownStanding(ctx, link.mailboxKey, ownerKey))) {
    return { kind: 'gone' }
  }
  const devices = await activeDevices(ctx, link.mailboxKey, link.userKey)
  if (devices.length >= MAX_DEVICE_PASSWORDS) {
    return { kind: 'limit' }
  }
  return null
}

/**
 * Redeem a link: open and unexpired -> download ticket (only its SHA-256 here), 60 seconds.
 * `ownerKey` is the mailbox's current owner (the app has checked it); link and data must
 * belong to them. `gone`: unknown, expired, already redeemed, different owner (the same for the
 * caller). `limit`: the mailbox already has the maximum number of devices; the link stays
 * open. Nothing is written on `gone` and `limit`.
 */
export const claim = mutation({
  args: { tokenHash: v.string(), ticketHash: v.string(), ownerKey: v.string() },
  returns: v.union(
    v.object({ kind: v.literal('claimed'), id: v.string(), expiresAt: v.number() }),
    v.object({ kind: v.literal('gone') }),
    v.object({ kind: v.literal('limit') }),
  ),
  handler: async (ctx, args) => {
    if (!isSha256Hex(args.ticketHash)) throw new Error('mailBridge: invalid hash')
    const now = Date.now()
    const link = await linkByToken(ctx, args.tokenHash)
    if (!link || link.state !== 'open' || link.expiresAt <= now) return { kind: 'gone' as const }
    const refused = await refuseUnusable(ctx, link, args.ownerKey)
    if (refused) return refused
    if (await linkByTicket(ctx, args.ticketHash)) throw new Error('mailBridge: ticket already used')
    const expiresAt = now + SETUP_TICKET_TTL_MS
    await ctx.db.patch('setupLinks', link._id, {
      state: 'claimed',
      ticketHash: args.ticketHash,
      claimedAt: now,
      expiresAt,
    })
    return { kind: 'claimed' as const, id: link._id, expiresAt }
  },
})

/**
 * Redeem a download ticket: create a device password (only its SHA-256 here) with the label from
 * the link and mark the link as done, in one transaction. `gone`: unknown, expired,
 * already redeemed, different owner, no longer enabled. `limit`: too many devices by now.
 * In both cases no device is created, and the ticket stays as it was.
 */
export const redeem = mutation({
  args: { ticketHash: v.string(), secretHash: v.string(), ownerKey: v.string() },
  returns: v.union(
    v.object({
      kind: v.literal('created'),
      id: v.string(),
      deviceId: v.string(),
      label: v.string(),
    }),
    v.object({ kind: v.literal('gone') }),
    v.object({ kind: v.literal('limit') }),
  ),
  handler: async (ctx, args) => {
    const now = Date.now()
    const link = await linkByTicket(ctx, args.ticketHash)
    if (!link || link.state !== 'claimed' || link.expiresAt <= now) {
      return { kind: 'gone' as const }
    }
    if (link.ownerKey !== args.ownerKey) return { kind: 'gone' as const }
    const device = await insertDevicePassword(ctx, {
      mailboxKey: link.mailboxKey,
      ownerKey: args.ownerKey,
      // The device belongs to whoever created the link (member or owner).
      ...(link.userKey !== undefined ? { userKey: link.userKey } : {}),
      label: link.label,
      secretHash: args.secretHash,
    })
    if (device.kind === 'limit') return { kind: 'limit' as const }
    if (device.kind !== 'created') return { kind: 'gone' as const }
    await ctx.db.patch('setupLinks', link._id, {
      state: 'downloaded',
      downloadedAt: now,
      deviceId: device.id,
    })
    return { kind: 'created' as const, id: link._id, deviceId: device.id, label: device.label }
  },
})

/**
 * State of a link for the dialog that created it: without hashes. The app checks that the link
 * belongs to its mailbox. `null`: unknown or already cleaned up.
 */
export const get = query({
  args: { id: v.string() },
  returns: v.union(
    v.null(),
    v.object({
      id: v.string(),
      mailboxKey: v.string(),
      ownerKey: v.string(),
      userKey: v.union(v.string(), v.null()),
      label: v.string(),
      state: setupStateValidator,
      expiresAt: v.number(),
      /** The device password created on download (`downloaded` only), for revoking in the dialog. */
      deviceId: v.union(v.string(), v.null()),
    }),
  ),
  handler: async (ctx, args) => {
    const id = ctx.db.normalizeId('setupLinks', args.id)
    const link = id ? await ctx.db.get('setupLinks', id) : null
    if (!link) return null
    return {
      id: link._id,
      mailboxKey: link.mailboxKey,
      ownerKey: link.ownerKey,
      userKey: link.userKey ?? null,
      label: link.label,
      state: link.state,
      expiresAt: link.expiresAt,
      deviceId: link.deviceId ?? null,
    }
  },
})

/**
 * Discard a link: it is invalid from now on, even if someone photographed the QR code.
 * "Cancel" in the dialog discards open links and those with a ticket. If someone merely closes
 * the dialog (`onlyOpen`), only a still-open link is affected: a device that is loading right now
 * should not fail midway. `false`: unknown, from a different mailbox, already downloaded or (with
 * `onlyOpen`) already redeemed, the same for the caller; a downloaded device is revoked in the
 * list.
 */
export const discard = mutation({
  args: {
    id: v.string(),
    mailboxKey: v.string(),
    /** Whose link: absent = the owner's. Another person's link is not affected. */
    userKey: v.optional(v.string()),
    onlyOpen: v.optional(v.boolean()),
  },
  returns: v.boolean(),
  handler: async (ctx, args) => {
    const id = ctx.db.normalizeId('setupLinks', args.id)
    const link = id ? await ctx.db.get('setupLinks', id) : null
    if (
      !link ||
      link.mailboxKey !== args.mailboxKey ||
      link.userKey !== args.userKey ||
      link.state === 'downloaded'
    ) {
      return false
    }
    if (args.onlyOpen && link.state !== 'open') return false
    const now = Date.now()
    if (link.expiresAt > now) await ctx.db.patch('setupLinks', link._id, { expiresAt: now })
    return true
  },
})
