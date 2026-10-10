// Setup links (`setup.ts`), shared helpers: which links can still become something, and
// invalidating them immediately. Here so that `devices.revokeAll` (disabling a mailbox) can use
// them without `devices.ts` and `setup.ts` importing each other.

import type { MutationCtx, QueryCtx } from '../_generated/server.js'
import { MAX_OPEN_SETUP_LINKS } from './access.js'

/**
 * Links of a mailbox that can still become a device: open, or with a valid session token.
 * At most `limit` per state (the per-mailbox cap keeps the number small).
 */
export async function pendingSetupLinks(
  ctx: QueryCtx,
  mailboxKey: string,
  now: number,
  limit = MAX_OPEN_SETUP_LINKS + 1,
) {
  const pending = []
  for (const state of ['open', 'claimed'] as const) {
    pending.push(
      ...(await ctx.db
        .query('setupLinks')
        .withIndex('by_mailboxKey_and_state_and_expiresAt', (q) =>
          q.eq('mailboxKey', mailboxKey).eq('state', state).gt('expiresAt', now),
        )
        .take(limit)),
    )
  }
  return pending
}

/**
 * Invalidate all links of a mailbox that could still become something, effective immediately
 * (the mailbox is being disabled). Enabling it again later does not bring them back: they have
 * expired. Returns how many.
 */
export async function expireSetupLinks(ctx: MutationCtx, mailboxKey: string, now: number) {
  // There can only be more than the cap if it was bypassed once; read generously.
  const pending = await pendingSetupLinks(ctx, mailboxKey, now, USER_LINK_WINDOW)
  for (const link of pending) await ctx.db.patch('setupLinks', link._id, { expiresAt: now })
  return pending.length
}

/**
 * At most this many links per state are read for the per-person cap. For shared mailboxes
 * `MAX_OPEN_SETUP_LINKS` applies per person; the app limits the members per mailbox, and the
 * window comfortably exceeds that.
 */
const USER_LINK_WINDOW = 200

/**
 * Links of ONE person on a mailbox that can still become a device (`userKey: undefined` = those
 * of the owner). The cap applies per person, so that a member with open links cannot lock out
 * the owner.
 */
export async function pendingUserSetupLinks(
  ctx: QueryCtx,
  mailboxKey: string,
  userKey: string | undefined,
  now: number,
) {
  const pending = await pendingSetupLinks(ctx, mailboxKey, now, USER_LINK_WINDOW)
  return pending.filter((link) => link.userKey === userKey)
}

/** Invalidate a member's links on a mailbox immediately (access revoked). */
export async function expireUserSetupLinks(
  ctx: MutationCtx,
  mailboxKey: string,
  userKey: string,
  now: number,
) {
  const pending = await pendingUserSetupLinks(ctx, mailboxKey, userKey, now)
  for (const link of pending) await ctx.db.patch('setupLinks', link._id, { expiresAt: now })
  return pending.length
}
