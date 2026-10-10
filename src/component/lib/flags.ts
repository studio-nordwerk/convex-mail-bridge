// Flags of a row (pure). What is stored is always the normal form: known system flags in their
// usual spelling, keywords as delivered, sorted, unique, capped.

export const SEEN = '\\Seen'

/** At most this many keywords per row; the five system flags always fit in addition. */
export const MAX_KEYWORDS = 32
const MAX_KEYWORD_CHARS = 64

/** `\Recent` is set only by the server per session; it is never stored. */
const SYSTEM_FLAGS = new Map([
  ['\\seen', SEEN],
  ['\\answered', '\\Answered'],
  ['\\flagged', '\\Flagged'],
  ['\\deleted', '\\Deleted'],
  ['\\draft', '\\Draft'],
])

function isSystemFlag(flag: string): boolean {
  return flag.startsWith('\\')
}

/** IMAP atom: printable ASCII without `( ) { % * " \ ]` and without spaces. */
function isKeyword(value: string): boolean {
  if (value.length === 0 || value.length > MAX_KEYWORD_CHARS) return false
  for (let index = 0; index < value.length; index++) {
    const code = value.charCodeAt(index)
    if (code <= 0x20 || code >= 0x7f) return false
    if ('(){%*"\\]'.includes(value[index])) return false
  }
  return true
}

/** Normal form of a flag, or `null` if it is not stored. */
export function canonicalFlag(flag: string): string | null {
  if (isSystemFlag(flag)) return SYSTEM_FLAGS.get(flag.toLowerCase()) ?? null
  return isKeyword(flag) ? flag : null
}

/** Sorted, unique, storable flags only, at most `MAX_KEYWORDS` keywords. */
export function normalizeFlags(flags: readonly string[]): string[] {
  const system = new Set<string>()
  const keywords = new Set<string>()
  for (const flag of flags) {
    const canonical = canonicalFlag(flag)
    if (!canonical) continue
    if (isSystemFlag(canonical)) system.add(canonical)
    else keywords.add(canonical)
  }
  return [...system, ...[...keywords].sort().slice(0, MAX_KEYWORDS)].sort()
}

export function hasSeen(flags: readonly string[]): boolean {
  return flags.includes(SEEN)
}

export function withSeen(flags: readonly string[], seen: boolean): string[] {
  const rest = flags.filter((flag) => flag !== SEEN)
  return normalizeFlags(seen ? [...rest, SEEN] : rest)
}

/**
 * Remove and set flags, in this order: whatever is in both lists is set afterwards. Beyond
 * `MAX_KEYWORDS` no keyword is added; existing ones stay.
 */
export function applyFlagChange(
  current: readonly string[],
  add: readonly string[],
  remove: readonly string[],
): string[] {
  const removed = new Set(normalizeFlags(remove))
  const next = new Set(normalizeFlags(current).filter((flag) => !removed.has(flag)))
  let keywords = [...next].filter((flag) => !isSystemFlag(flag)).length
  for (const flag of normalizeFlags(add)) {
    if (next.has(flag)) continue
    if (!isSystemFlag(flag)) {
      if (keywords >= MAX_KEYWORDS) continue
      keywords += 1
    }
    next.add(flag)
  }
  return [...next].sort()
}

export function sameFlags(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((flag, index) => flag === b[index])
}

/**
 * What a row contributes to the counters of its folder, as the difference between before and
 * after. `null` = the row does not exist (yet) or is no longer live.
 */
export function counterDelta(
  before: { seen: boolean } | null,
  after: { seen: boolean } | null,
): { exists: number; unseen: number } {
  const exists = (after ? 1 : 0) - (before ? 1 : 0)
  const unseen = (after && !after.seen ? 1 : 0) - (before && !before.seen ? 1 : 0)
  return { exists, unseen }
}
