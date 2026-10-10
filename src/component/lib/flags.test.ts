import { describe, expect, it } from 'vitest'
import {
  MAX_KEYWORDS,
  applyFlagChange,
  canonicalFlag,
  counterDelta,
  hasSeen,
  normalizeFlags,
  sameFlags,
  withSeen,
} from './flags.js'

describe('mailBridge: flags', () => {
  it('system flags get their usual spelling, unknown ones and \\Recent are dropped', () => {
    expect(canonicalFlag('\\seen')).toBe('\\Seen')
    expect(canonicalFlag('\\FLAGGED')).toBe('\\Flagged')
    expect(canonicalFlag('\\Recent')).toBeNull()
    expect(canonicalFlag('\\Invented')).toBeNull()
  })

  it('keywords: IMAP atoms only, at most 64 characters', () => {
    expect(canonicalFlag('$Forwarded')).toBe('$Forwarded')
    expect(canonicalFlag('$MailFlagBit0')).toBe('$MailFlagBit0')
    expect(canonicalFlag('')).toBeNull()
    expect(canonicalFlag('two wörds')).toBeNull()
    expect(canonicalFlag('with(paren')).toBeNull()
    expect(canonicalFlag('star*')).toBeNull()
    expect(canonicalFlag('line\r\nA1 LOGOUT')).toBeNull()
    expect(canonicalFlag('ü')).toBeNull()
    expect(canonicalFlag('a'.repeat(64))).toBe('a'.repeat(64))
    expect(canonicalFlag('a'.repeat(65))).toBeNull()
  })

  it('normal form is sorted and unique', () => {
    expect(normalizeFlags(['b', '\\seen', 'a', '\\Seen', 'b', '\\Recent'])).toEqual([
      '\\Seen',
      'a',
      'b',
    ])
    expect(normalizeFlags([])).toEqual([])
  })

  it('at most 32 keywords, system flags always fit', () => {
    const many = Array.from({ length: 40 }, (_, index) => `k${String(index).padStart(2, '0')}`)
    const flags = normalizeFlags([...many, '\\Seen', '\\Flagged'])
    expect(flags.filter((flag) => !flag.startsWith('\\'))).toHaveLength(MAX_KEYWORDS)
    expect(flags).toContain('\\Seen')
    expect(flags).toContain('\\Flagged')
    // Full of keywords: `\Seen` can still be set, one more keyword cannot.
    const full = normalizeFlags(many)
    expect(hasSeen(withSeen(full, true))).toBe(true)
    expect(applyFlagChange(full, ['new', '\\Answered'], [])).toEqual([...full, '\\Answered'].sort())
  })

  it('withSeen sets and removes only \\Seen', () => {
    expect(withSeen(['\\Flagged'], true)).toEqual(['\\Flagged', '\\Seen'])
    expect(withSeen(['\\Flagged', '\\Seen'], false)).toEqual(['\\Flagged'])
    expect(withSeen(['\\Seen'], true)).toEqual(['\\Seen'])
  })

  it('applyFlagChange: remove first, then set', () => {
    expect(applyFlagChange(['\\Seen', 'a'], ['\\flagged'], ['a'])).toEqual(['\\Flagged', '\\Seen'])
    expect(applyFlagChange(['a'], ['a'], ['a'])).toEqual(['a'])
    expect(applyFlagChange(['a'], [], ['does-not-exist'])).toEqual(['a'])
  })

  it('sameFlags compares the normal form position by position', () => {
    expect(sameFlags(['a', 'b'], ['a', 'b'])).toBe(true)
    expect(sameFlags(['a'], ['a', 'b'])).toBe(false)
    expect(sameFlags(['a', 'b'], ['a', 'c'])).toBe(false)
  })

  it('counterDelta: contribution of a row to exists and unseen', () => {
    expect(counterDelta(null, { seen: false })).toEqual({ exists: 1, unseen: 1 })
    expect(counterDelta(null, { seen: true })).toEqual({ exists: 1, unseen: 0 })
    expect(counterDelta({ seen: false }, { seen: true })).toEqual({ exists: 0, unseen: -1 })
    expect(counterDelta({ seen: true }, { seen: false })).toEqual({ exists: 0, unseen: 1 })
    expect(counterDelta({ seen: false }, null)).toEqual({ exists: -1, unseen: -1 })
    expect(counterDelta({ seen: true }, null)).toEqual({ exists: -1, unseen: 0 })
    expect(counterDelta({ seen: true }, { seen: true })).toEqual({ exists: 0, unseen: 0 })
  })
})
