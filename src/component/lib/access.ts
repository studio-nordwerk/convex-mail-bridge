// Mail client sign-in (pure): limits, throttle for failed attempts, shape of the hashes.
//
// The throttle counts invalid attempts per key in a fixed window that starts with the first failed
// attempt. Once the limit is reached, a further invalid attempt is rejected without a write until
// the window ends (a locked-out attacker then costs only reads).
//
// A valid device password always signs in, whatever the counter says: with 192 bits of randomness
// the throttle protects not the password but compute and table. So a device that repeats a stale
// password never locks out a second device behind the same IP.

/** At most this many active device passwords per mailbox. */
export const MAX_DEVICE_PASSWORDS = 10
export const MAX_LABEL_CHARS = 40
/** A session token is valid this long. */
export const SESSION_TTL_MS = 12 * 60 * 60 * 1000
/** `lastUsedAt` is updated at most this often (no write on every sign-in). */
export const LAST_USED_STEP_MS = 60 * 60 * 1000

export const THROTTLE_WINDOW_MS = 15 * 60 * 1000
/** Failed attempts per pair of address and client IP in the window. */
export const PAIR_LIMIT = 10
/** Failed attempts per client IP in the window. */
export const IP_LIMIT = 30
/** Throttle keys are hashes made by the app; longer ones are rejected. */
export const MAX_THROTTLE_KEY_CHARS = 128
/**
 * The throttle holds at most this many rows. Beyond that no new one is created, and an invalid
 * attempt that would need one is rejected without a write until cleanup makes room.
 */
export const MAX_ATTEMPT_ROWS = 2000
/** Revoked device passwords stay this long, then cleanup deletes them. */
export const REVOKED_RETENTION_MS = 30 * 24 * 60 * 60 * 1000

// Setup by QR code (`setup.ts`).
/** A setup link is valid this long and can be redeemed once. */
export const SETUP_LINK_TTL_MS = 10 * 60 * 1000
/** A download ticket (from a redeemed link) is valid this long and once. */
export const SETUP_TICKET_TTL_MS = 60 * 1000
/** At most this many links per mailbox that can still become something (open or with a ticket). */
export const MAX_OPEN_SETUP_LINKS = 3
/**
 * Expired links stay this long, then the hourly cleanup deletes them. Until then the dialog that
 * created the link still shows what became of it.
 */
export const SETUP_LINK_RETENTION_MS = 60 * 60 * 1000

/** SHA-256 as lowercase hex: the only form in which secrets come in. */
export function isSha256Hex(value: string): boolean {
  return /^[0-9a-f]{64}$/.test(value)
}

export function isThrottleKey(value: string): boolean {
  return value.length > 0 && value.length <= MAX_THROTTLE_KEY_CHARS
}

export type AttemptWindow = { windowStart: number; count: number }

/** Is the window of this row still running? */
export function windowActive(row: AttemptWindow | null, now: number): row is AttemptWindow {
  return row !== null && now - row.windowStart < THROTTLE_WINDOW_MS
}

/** Failed attempts in the current window (0 if there is none or it has expired). */
export function attemptsInWindow(row: AttemptWindow | null, now: number): number {
  return windowActive(row, now) ? row.count : 0
}

/**
 * Seconds until the lock ends, or `null` if the key is not locked. At least one second, so a
 * caller never reads "immediately again".
 */
export function retryAfterSeconds(
  row: AttemptWindow | null,
  limit: number,
  now: number,
): number | null {
  if (!windowActive(row, now) || row.count < limit) return null
  return Math.max(1, Math.ceil((row.windowStart + THROTTLE_WINDOW_MS - now) / 1000))
}

/** The row after a further failed attempt: counts in the current window or starts a new one. */
export function afterFailure(row: AttemptWindow | null, now: number): AttemptWindow {
  if (windowActive(row, now)) return { windowStart: row.windowStart, count: row.count + 1 }
  return { windowStart: now, count: 1 }
}

/**
 * Label of a device: control characters and line breaks become spaces, then trimmed; 1 to
 * `MAX_LABEL_CHARS` characters, otherwise `null`.
 */
export function cleanLabel(value: string): string | null {
  let plain = ''
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0
    plain += code < 0x20 || code === 0x7f ? ' ' : char
  }
  const label = plain.replace(/\s+/g, ' ').trim()
  if (label.length === 0 || label.length > MAX_LABEL_CHARS) return null
  return label
}
