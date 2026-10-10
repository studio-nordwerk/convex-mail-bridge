import { describe, expect, it } from 'vitest'
import {
  IP_LIMIT,
  MAX_LABEL_CHARS,
  MAX_THROTTLE_KEY_CHARS,
  PAIR_LIMIT,
  THROTTLE_WINDOW_MS,
  afterFailure,
  attemptsInWindow,
  cleanLabel,
  isSha256Hex,
  isThrottleKey,
  retryAfterSeconds,
} from './access.js'

const T0 = 1_800_000_000_000

describe('Throttle (pure)', () => {
  it('counts in the window from the first failed attempt and starts a new one afterwards', () => {
    let row = afterFailure(null, T0)
    expect(row).toEqual({ windowStart: T0, count: 1 })
    row = afterFailure(row, T0 + 60_000)
    expect(row).toEqual({ windowStart: T0, count: 2 })
    expect(attemptsInWindow(row, T0 + THROTTLE_WINDOW_MS - 1)).toBe(2)
    // Once the window expires the row no longer counts; the next failed attempt starts afresh.
    expect(attemptsInWindow(row, T0 + THROTTLE_WINDOW_MS)).toBe(0)
    expect(afterFailure(row, T0 + THROTTLE_WINDOW_MS)).toEqual({
      windowStart: T0 + THROTTLE_WINDOW_MS,
      count: 1,
    })
  })

  it('locks from the limit until the window ends and reports the wait time', () => {
    const below = { windowStart: T0, count: PAIR_LIMIT - 1 }
    const full = { windowStart: T0, count: PAIR_LIMIT }
    expect(retryAfterSeconds(null, PAIR_LIMIT, T0)).toBeNull()
    expect(retryAfterSeconds(below, PAIR_LIMIT, T0 + 1000)).toBeNull()
    expect(retryAfterSeconds(full, PAIR_LIMIT, T0 + 1000)).toBe(THROTTLE_WINDOW_MS / 1000 - 1)
    // Never "immediately again": at least one second, even shortly before expiry.
    expect(retryAfterSeconds(full, PAIR_LIMIT, T0 + THROTTLE_WINDOW_MS - 1)).toBe(1)
    expect(retryAfterSeconds(full, PAIR_LIMIT, T0 + THROTTLE_WINDOW_MS)).toBeNull()
    // The same row is still free for the more generous per-IP limit.
    expect(retryAfterSeconds(full, IP_LIMIT, T0 + 1000)).toBeNull()
  })

  it('accepts only SHA-256 in hex as a hash and only bounded strings as a key', () => {
    expect(isSha256Hex('a'.repeat(64))).toBe(true)
    expect(isSha256Hex('A'.repeat(64))).toBe(false)
    expect(isSha256Hex('a'.repeat(63))).toBe(false)
    expect(isSha256Hex('')).toBe(false)
    expect(isThrottleKey('p:abc')).toBe(true)
    expect(isThrottleKey('')).toBe(false)
    expect(isThrottleKey('x'.repeat(MAX_THROTTLE_KEY_CHARS))).toBe(true)
    expect(isThrottleKey('x'.repeat(MAX_THROTTLE_KEY_CHARS + 1))).toBe(false)
  })

  it('label: trimmed, without control characters, 1 to 40 characters', () => {
    expect(cleanLabel('  iPhone  ')).toBe('iPhone')
    expect(cleanLabel('Mac\n\tin  Büro')).toBe('Mac in Büro')
    expect(cleanLabel('a\u0000b')).toBe('a b')
    expect(cleanLabel('   ')).toBeNull()
    expect(cleanLabel('x'.repeat(MAX_LABEL_CHARS))).toBe('x'.repeat(MAX_LABEL_CHARS))
    expect(cleanLabel('x'.repeat(MAX_LABEL_CHARS + 1))).toBeNull()
  })
})
