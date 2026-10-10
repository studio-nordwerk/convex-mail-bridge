import { describe, expect, test } from 'vitest'
import {
  BRIDGE_LIMITS,
  DEFAULT_FOLDER_NAMES,
  SUBMIT_REJECTIONS,
  asciiMessage,
  base32,
  bridgeAuthorized,
  bridgeSubmitRejection,
  folderKey,
  inBatches,
  isTokenShape,
  namesSeenFlag,
  newDevicePassword,
  newToken,
  normalizeMessageId,
  parseAppend,
  parseChanges,
  parseEntries,
  parseEnvelope,
  parseLogin,
  parseRaw,
  parseSeq,
  parseStore,
  readJsonObject,
  secretHash,
  throttleIp,
  throttleKeys,
  usableSecret,
} from './edge.js'

const SECRET = 'a-shared-secret-with-at-least-32-chars'

describe('secret', () => {
  test('a secret needs 32 characters, surrounding whitespace does not count', () => {
    expect(usableSecret(undefined)).toBeNull()
    expect(usableSecret('short')).toBeNull()
    expect(usableSecret(` ${'x'.repeat(31)} `)).toBeNull()
    expect(usableSecret(` ${SECRET}\n`)).toBe(SECRET)
  })

  test('only the exact bearer header is accepted', async () => {
    expect(await bridgeAuthorized(`Bearer ${SECRET}`, SECRET)).toBe(true)
    expect(await bridgeAuthorized(`bearer ${SECRET}`, SECRET)).toBe(false)
    expect(await bridgeAuthorized(`Bearer ${SECRET}x`, SECRET)).toBe(false)
    expect(await bridgeAuthorized(null, SECRET)).toBe(false)
    expect(await bridgeAuthorized('Bearer ', null)).toBe(false)
  })
})

describe('tokens and device passwords', () => {
  test('a token is 43 base64url characters and never repeats', () => {
    const tokens = new Set(Array.from({ length: 50 }, () => newToken()))
    expect(tokens.size).toBe(50)
    for (const token of tokens) expect(isTokenShape(token)).toBe(true)
    expect(isTokenShape('a'.repeat(42))).toBe(false)
    expect(isTokenShape(`${'a'.repeat(42)}+`)).toBe(false)
  })

  test('a device password is the prefix and 39 base32 characters', async () => {
    expect(newDevicePassword()).toMatch(/^mb_[a-z2-7]{39}$/)
    expect(newDevicePassword('acme_')).toMatch(/^acme_[a-z2-7]{39}$/)
    expect(await secretHash('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  })

  test('base32 follows RFC 4648 (lowercase, no padding)', () => {
    const bytes = (text: string) => new TextEncoder().encode(text)
    expect(base32(bytes(''))).toBe('')
    expect(base32(bytes('f'))).toBe('my')
    expect(base32(bytes('fo'))).toBe('mzxq')
    expect(base32(bytes('foobar'))).toBe('mzxw6ytboi')
  })
})

describe('throttle', () => {
  test('IPv4 as a whole, IPv6 as its /64, mapped IPv4 as IPv4', () => {
    expect(throttleIp('192.0.2.7')).toBe('192.0.2.7')
    expect(throttleIp('2001:db8:1:2:3:4:5:6')).toBe('2001:db8:1:2::/64')
    expect(throttleIp('2001:db8::1')).toBe('2001:db8:0:0::/64')
    expect(throttleIp('::ffff:192.0.2.7')).toBe('192.0.2.7')
  })

  test('anything that is not a bare address is refused', () => {
    for (const value of ['', 'localhost', '192.0.2.256', '192.0.2.7:993', '[2001:db8::1]', 'fe80::1%eth0', '1:2:3:4:5:6:7:8:9']) {
      expect(throttleIp(value), value).toBeNull()
    }
  })

  test('keys depend on secret, address and IP and reveal neither', async () => {
    const keys = await throttleKeys(SECRET, 'ada@example.com', '192.0.2.7')
    expect(keys.pairKey).toMatch(/^p:[0-9a-f]{64}$/)
    expect(keys.ipKey).toMatch(/^i:[0-9a-f]{64}$/)
    const other = await throttleKeys(SECRET, 'grace@example.com', '192.0.2.7')
    expect(other.ipKey).toBe(keys.ipKey)
    expect(other.pairKey).not.toBe(keys.pairKey)
    expect((await throttleKeys(`${SECRET}!`, 'ada@example.com', '192.0.2.7')).ipKey).not.toBe(keys.ipKey)
  })
})

describe('folders', () => {
  test('names map to ledger folders, composed or decomposed', () => {
    expect(folderKey('INBOX', DEFAULT_FOLDER_NAMES)).toBe('INBOX')
    expect(folderKey('Sent', DEFAULT_FOLDER_NAMES)).toBe('Sent')
    expect(folderKey('inbox', DEFAULT_FOLDER_NAMES)).toBeNull()
    const names = { ...DEFAULT_FOLDER_NAMES, Later: { name: 'Später' } }
    expect(folderKey('Später', names)).toBe('Later')
    expect(folderKey('Später', names)).toBe('Later')
    expect(folderKey('Later', names)).toBeNull()
  })
})

describe('requests', () => {
  test('login: normalizes the address and refuses unknown fields and bad IPs', () => {
    const ok = parseLogin({ address: ' Ada@Example.com ', password: 'pw', clientIp: '192.0.2.7' })
    expect(ok).toEqual({ ok: true, value: { address: 'ada@example.com', password: 'pw', ip: '192.0.2.7', bridge: 1 } })
    expect(parseLogin({ address: 'a@example.com', password: 'pw', clientIp: '192.0.2.7', bridge: 2 })).toMatchObject({ value: { bridge: 2 } })
    for (const body of [
      { address: 'a@example.com', password: 'pw', clientIp: '192.0.2.7', mailbox: 'x' },
      { address: 'a@example.com', password: 'pw', clientIp: 'nope' },
      { address: 'a@example.com', password: '', clientIp: '192.0.2.7' },
      { address: 'a@example.com', password: 'p'.repeat(257), clientIp: '192.0.2.7' },
      { address: 'a@example.com', password: 'pw', clientIp: '192.0.2.7', bridge: 0 },
    ]) {
      expect(parseLogin(body)).toEqual({ ok: false, status: 400, code: 'request' })
    }
  })

  test('entries, changes, raw: integers in range only', () => {
    expect(parseEntries({ folder: 'INBOX', limit: 2000 })).toEqual({ ok: true, value: { folder: 'INBOX', limit: 2000 } })
    expect(parseEntries({ folder: 'INBOX', limit: 10, afterUid: 0 })).toMatchObject({ value: { afterUid: 0 } })
    expect(parseEntries({ folder: 'INBOX', limit: 2001 }).ok).toBe(false)
    expect(parseEntries({ folder: 'INBOX', limit: 1.5 }).ok).toBe(false)
    expect(parseEntries({ folder: 'x'.repeat(65), limit: 1 }).ok).toBe(false)
    expect(parseChanges({ folder: 'INBOX', sinceModseq: 0, limit: 1 }).ok).toBe(true)
    expect(parseChanges({ folder: 'INBOX', sinceModseq: -1, limit: 1 }).ok).toBe(false)
    expect(parseRaw({ folder: 'INBOX', uid: 1, uidValidity: 4294967295 }).ok).toBe(true)
    expect(parseRaw({ folder: 'INBOX', uid: 0, uidValidity: 1 }).ok).toBe(false)
    expect(parseRaw({ folder: 'INBOX', uid: 1, uidValidity: 4294967296 }).ok).toBe(false)
  })

  test('store: missing flag lists are empty, limits hold', () => {
    expect(parseStore({ folder: 'INBOX', uids: [1, 2] })).toEqual({ ok: true, value: { folder: 'INBOX', uids: [1, 2], add: [], remove: [] } })
    expect(parseStore({ folder: 'INBOX', uids: [1], add: null, remove: ['\\Seen'] }).ok).toBe(true)
    expect(parseStore({ folder: 'INBOX', uids: Array.from({ length: BRIDGE_LIMITS.uids + 1 }, (_, i) => i + 1) }).ok).toBe(false)
    expect(parseStore({ folder: 'INBOX', uids: [1], add: ['x'.repeat(101)] }).ok).toBe(false)
    expect(parseStore({ folder: 'INBOX', uids: ['1'] }).ok).toBe(false)
    expect(namesSeenFlag(['\\SEEN'])).toBe(true)
    expect(namesSeenFlag(['\\Flagged', '$Forwarded'])).toBe(false)
  })

  test('append: the Message-ID loses its brackets, the digest needs its shape', () => {
    const digest = 'A'.repeat(64)
    expect(parseAppend({ folder: 'Sent', messageId: ' <id@example.com> ', digest })).toEqual({
      ok: true,
      value: { folder: 'Sent', messageId: 'id@example.com', digest: 'a'.repeat(64) },
    })
    expect(parseAppend({ folder: 'Sent', messageId: '<>', digest }).ok).toBe(false)
    expect(parseAppend({ folder: 'Sent', messageId: 'id@example.com', digest: 'abc' }).ok).toBe(false)
    expect(normalizeMessageId(`<${'x'.repeat(501)}>`)).toBeUndefined()
  })

  test('seq: an entry that cannot be a token keeps its place', () => {
    const token = newToken()
    expect(parseSeq({ sessions: [token, 'nope'] })).toEqual({ ok: true, value: { sessions: [token, ''] } })
    expect(parseSeq({ sessions: [1] }).ok).toBe(false)
    expect(parseSeq({ sessions: Array.from({ length: 201 }, () => token) }).ok).toBe(false)
  })

  test('envelope: one sender, a JSON list of recipients', () => {
    expect(parseEnvelope('Ada@Example.com', '["Grace@Example.com"]')).toEqual({
      ok: true,
      value: { from: 'ada@example.com', to: ['grace@example.com'] },
    })
    expect(parseEnvelope(null, '["a@example.com"]').ok).toBe(false)
    expect(parseEnvelope('a@example.com', 'a@example.com').ok).toBe(false)
    expect(parseEnvelope('a@example.com', '[]').ok).toBe(false)
    expect(parseEnvelope('a@example.com', '[1]').ok).toBe(false)
  })

  test('bodies: JSON objects only, strict UTF-8, at most 64 KiB', async () => {
    const request = (body: BodyInit) => new Request('https://example.com', { method: 'POST', body })
    expect(await readJsonObject(request('{"a":1}'))).toEqual({ ok: true, value: { a: 1 } })
    expect(await readJsonObject(request('[1]'))).toMatchObject({ status: 400 })
    expect(await readJsonObject(request('not json'))).toMatchObject({ status: 400 })
    expect(await readJsonObject(request(new Uint8Array([0x7b, 0x22, 0xff, 0x22, 0x3a, 0x31, 0x7d])))).toMatchObject({ status: 400 })
    expect(await readJsonObject(request(JSON.stringify({ a: 'x'.repeat(BRIDGE_LIMITS.jsonBytes) })))).toMatchObject({ status: 413 })
  })

  test('batches', () => {
    expect(inBatches([1, 2, 3, 4, 5], 2)).toEqual([[1, 2], [3, 4], [5]])
    expect(inBatches([], 2)).toEqual([[]])
  })
})

describe('submission rejections', () => {
  test('every rejection carries an SMTP code and a plain ASCII text', async () => {
    for (const reason of Object.keys(SUBMIT_REJECTIONS) as Array<keyof typeof SUBMIT_REJECTIONS>) {
      const response = bridgeSubmitRejection(reason)
      expect(response.status).toBe(SUBMIT_REJECTIONS[reason].status)
      expect(response.status).not.toBe(500)
      const body = (await response.json()) as { error: string; smtpCode: number; message: string }
      expect(body.error).toBe(reason)
      expect([451, 550, 552, 554]).toContain(body.smtpCode)
      expect(body.message).toMatch(/^[\x20-\x7e]+$/)
    }
  })

  test('a custom text is reduced to ASCII', async () => {
    expect(asciiMessage('Größe \u{1f600}')).toBe('Gr??e ??')
    const body = (await bridgeSubmitRejection('rejected', 'Empfänger gesperrt').json()) as { message: string }
    expect(body.message).toBe('Empf?nger gesperrt')
  })
})
