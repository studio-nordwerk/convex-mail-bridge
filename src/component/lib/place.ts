// Placing a message in a folder: the one rule by which a message gets a row, changes folder, or
// stays where it is. `entries.add` and `entries.moveGroup` both go through here.
//
// What counts is the current row of the message (`findEntry`):
// - none: new row in the target folder (only with the details of a new message);
// - live in the target folder: nothing;
// - live in another folder: `move` tombstone there, new row with a new UID in the target;
// - `client` tombstone in the target folder: nothing, the message stays hidden there;
// - `client` tombstone in another folder: it becomes a trail (`move`), new row in the target;
// - `purge` tombstone: nothing, the message no longer exists.

import type { MutationCtx } from '../_generated/server.js'
import type { Doc } from '../_generated/dataModel.js'
import { hasSeen, withSeen } from './flags.js'
import type { Folder, openLedger } from './ledger.js'

type Ledger = ReturnType<typeof openLedger>

export type Fresh = { size: number; internalDate: number; seen: boolean; flags: string[] }
export type Tag = { groupKey?: string; kind?: 'in' | 'out' }

export type Placed =
  | { change: 'none'; entry: Doc<'entries'> | null }
  | { change: 'created'; uid: number }
  | { change: 'moved'; uid: number; from: { folder: Folder; uid: number; live: boolean } }

/**
 * Applies the rule to a message. `current` is its current row (or `null`), `fresh` exists only
 * when the caller is adding a new message. The caller then calls `ledger.commit()`.
 */
export async function placeEntry(
  ctx: MutationCtx,
  ledger: Ledger,
  args: {
    mailboxKey: string
    messageKey: string
    target: Folder
    current: Doc<'entries'> | null
    fresh?: Fresh
    tag?: Tag
    now: number
  },
): Promise<Placed> {
  const { current, target } = args
  if (!current) {
    if (!args.fresh) return { change: 'none', entry: null }
    const uid = await ledger.takeUid(target)
    const modseq = await ledger.change(target, null, { seen: args.fresh.seen })
    await ctx.db.insert('entries', {
      mailboxKey: args.mailboxKey,
      folder: target,
      uid,
      messageKey: args.messageKey,
      modseq,
      size: args.fresh.size,
      internalDate: args.fresh.internalDate,
      flags: withSeen(args.fresh.flags, args.fresh.seen),
      live: true,
      ...tagFields(args.tag),
    })
    return { change: 'created', uid }
  }
  if (
    current.mailboxKey !== args.mailboxKey ||
    current.removedBy === 'purge' ||
    current.folder === target
  ) {
    return { change: 'none', entry: current }
  }

  const seen = hasSeen(current.flags)
  if (current.live) {
    const removedModseq = await ledger.change(current.folder, { seen }, null)
    await ctx.db.patch('entries', current._id, {
      live: false,
      modseq: removedModseq,
      removedAt: args.now,
      removedModseq,
      removedBy: 'move',
    })
  } else {
    // Already hidden: nothing changes in the old folder for mail clients, the stamp of the
    // removal stays. The trail's retention period starts now, though; otherwise `cleanup` would
    // sweep it right away and `entries.resolve` could no longer get from the old UID to the
    // message.
    await ctx.db.patch('entries', current._id, { removedBy: 'move', removedAt: args.now })
  }
  const uid = await ledger.takeUid(target)
  const modseq = await ledger.change(target, null, { seen })
  await ctx.db.insert('entries', {
    mailboxKey: current.mailboxKey,
    folder: target,
    uid,
    messageKey: current.messageKey,
    modseq,
    size: current.size,
    internalDate: current.internalDate,
    flags: current.flags,
    live: true,
    ...tagFields({
      groupKey: args.tag?.groupKey ?? current.groupKey,
      kind: args.tag?.kind ?? current.kind,
    }),
  })
  return {
    change: 'moved',
    uid,
    from: { folder: current.folder, uid: current.uid, live: current.live },
  }
}

export function tagFields(tag: Tag | undefined) {
  return {
    ...(tag?.groupKey !== undefined ? { groupKey: tag.groupKey } : {}),
    ...(tag?.kind !== undefined ? { kind: tag.kind } : {}),
  }
}

/** Does a row already carry exactly this group and kind? Missing values change nothing. */
export function tagDiffers(entry: Doc<'entries'>, tag: Tag | undefined): boolean {
  return (
    (tag?.groupKey !== undefined && tag.groupKey !== entry.groupKey) ||
    (tag?.kind !== undefined && tag.kind !== entry.kind)
  )
}
