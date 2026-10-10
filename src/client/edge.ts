// The HTTP edge between the bridge (IMAP and SMTP for mail clients) and Convex, pure parts: the
// shared secret, the shape of session tokens and device passwords, throttle keys, strict request
// parsing and the responses. The contract is docs/edge-contract.md; the routes and the functions
// behind them are in ./index.ts.
//
// Nothing in this file writes a secret, an address or an IP to a log or into an error message.

export const BRIDGE_BASE = '/bridge/v1'

/** Ledger folders. `INBOX` and `Sent` exist for every enabled mailbox. */
export const FOLDER_KEYS = ['INBOX', 'Sent', 'Archive', 'Trash', 'Later'] as const
export type Folder = (typeof FOLDER_KEYS)[number]

export type FolderName = { name: string; specialUse?: string }

/** How folders are called over the edge and in the mail client. Names can be overridden. */
export const DEFAULT_FOLDER_NAMES: Record<Folder, FolderName> = {
  INBOX: { name: 'INBOX' },
  Sent: { name: 'Sent', specialUse: '\\Sent' },
  Archive: { name: 'Archive', specialUse: '\\Archive' },
  Trash: { name: 'Trash', specialUse: '\\Trash' },
  Later: { name: 'Later' },
}

/** Folders beyond the two every mailbox has. A mailbox with any of them needs bridge version 2. */
export function isExtraFolder(folder: Folder): boolean {
  return folder !== 'INBOX' && folder !== 'Sent'
}

/**
 * The ledger folder for a name from a request, or `null`. Names are compared after Unicode
 * normalization (NFC): depending on the mail client an umlaut arrives composed or decomposed.
 */
export function folderKey(name: string, names: Record<Folder, FolderName>): Folder | null {
  const wanted = name.normalize('NFC')
  return FOLDER_KEYS.find((key) => names[key].name.normalize('NFC') === wanted) ?? null
}

/** Bridge version from which it can handle more than `INBOX` and `Sent` (`bridge` in `/login`). */
export const BRIDGE_VERSION_FOLDERS = 2

export const BRIDGE_LIMITS = {
  /** Body of a JSON request (500 UIDs or 200 sessions stay far below). */
  jsonBytes: 64 * 1024,
  /** `limit` of `/entries` and `/changes`. */
  rows: 2000,
  /** UIDs per `/store` and `/expunge`. */
  uids: 500,
  /** Flags per list and characters per flag in `/store`. */
  flags: 64,
  flagChars: 100,
  /** Sessions per `/seq`. */
  sessions: 200,
  addressChars: 320,
  /** Entries of `X-Envelope-To` that are read at all. */
  envelopeRecipients: 1000,
  passwordChars: 256,
  messageIdChars: 1000,
  folderChars: 64,
  ipChars: 64,
  /** Raw message of `/submit`. */
  submitBytes: 5 * 1024 * 1024,
} as const

/** Ports the bridge is reached on by mail clients: both with TLS from the first byte. */
export const BRIDGE_PORTS = { imap: 993, smtp: 465 } as const

/** Highest UID in IMAP (unsigned 32 bit). */
const MAX_UID = 0xffff_ffff

// ─── Secret ─────────────────────────────────────────────────────────────

/** A shorter secret counts as not set: a typo must not open the edge. */
export const MIN_BRIDGE_SECRET_CHARS = 32

/** The usable secret, or `null`: then the whole edge is off (every route answers 401). */
export function usableSecret(value: string | undefined | null): string | null {
  const secret = value?.trim() ?? ''
  return secret.length >= MIN_BRIDGE_SECRET_CHARS ? secret : null
}

async function sha256Bytes(value: string): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value)))
}

/**
 * Bytes in a plain `ArrayBuffer`, which is what `Blob`, `Response` and `crypto.subtle` take. Written
 * as the return type of `slice` and not as `Uint8Array<ArrayBuffer>`, because `Uint8Array` takes a
 * type argument only from TypeScript 5.7 on: this way the declarations of the package also compile
 * with an older compiler, where the type is simply `Uint8Array`.
 */
export type RawBytes = ReturnType<Uint8Array['slice']>

/** SHA-256 as lowercase hex. */
export async function sha256Hex(value: string | Uint8Array): Promise<string> {
  const bytes = typeof value === 'string' ? new TextEncoder().encode(value) : value
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes as RawBytes))
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

/** Compares two byte strings of equal length without stopping at the first difference. */
export function equalBytesConstantTime(a: Uint8Array, b: Uint8Array): boolean {
  if (a.byteLength !== b.byteLength) return false
  let difference = 0
  for (let index = 0; index < a.byteLength; index++) difference |= a[index] ^ b[index]
  return difference === 0
}

/**
 * `Authorization: Bearer <secret>`. Compared are the SHA-256 of both sides (equal length, the secret
 * itself is never compared byte by byte), in constant time. Never true without a secret.
 */
export async function bridgeAuthorized(header: string | null, secret: string | null) {
  if (!secret) return false
  const [given, expected] = await Promise.all([
    sha256Bytes(header ?? ''),
    sha256Bytes(`Bearer ${secret}`),
  ])
  return equalBytesConstantTime(given, expected)
}

// ─── Secrets: session token and device password ─────────────────────────

function randomBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  crypto.getRandomValues(bytes)
  return bytes
}

/** 256 bit as base64url (43 characters). Create in actions only (cryptographic randomness). */
export function newToken(): string {
  let binary = ''
  for (const byte of randomBytes(32)) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** Shape of a session token (checked before hashing, so that garbage is not even looked up). */
export function isTokenShape(value: string): boolean {
  return /^[A-Za-z0-9_-]{43}$/.test(value)
}

/** Base32 after RFC 4648, lowercase: no 0, 1, 8 and 9, so nothing is confused when typing. */
const BASE32 = 'abcdefghijklmnopqrstuvwxyz234567'

export function base32(bytes: Uint8Array): string {
  let output = ''
  let buffer = 0
  let bits = 0
  for (const byte of bytes) {
    buffer = (buffer << 8) | byte
    bits += 8
    while (bits >= 5) {
      output += BASE32[(buffer >>> (bits - 5)) & 31]
      bits -= 5
    }
    buffer &= (1 << bits) - 1
  }
  if (bits > 0) output += BASE32[(buffer << (5 - bits)) & 31]
  return output
}

export const DEFAULT_DEVICE_PASSWORD_PREFIX = 'mb_'

/**
 * A device password: the prefix and 192 bit of randomness as base32 (39 characters). Create in
 * actions only (cryptographic randomness); only its SHA-256 is ever stored.
 */
export function newDevicePassword(prefix: string = DEFAULT_DEVICE_PASSWORD_PREFIX): string {
  return `${prefix}${base32(randomBytes(24))}`
}

/** SHA-256 of a secret (device password or token): the only form that is stored. */
export async function secretHash(secret: string): Promise<string> {
  return await sha256Hex(secret)
}

// ─── Addresses and message ids ──────────────────────────────────────────

export function normalizeAddress(value: string): string {
  return value.trim().toLowerCase()
}

/** A Message-ID without angle brackets, or `undefined` when empty or longer than 500 characters. */
export function normalizeMessageId(value: string | undefined | null): string | undefined {
  if (!value) return undefined
  const trimmed = value.trim().replace(/^<+/, '').replace(/>+$/, '').trim()
  return trimmed && trimmed.length <= 500 ? trimmed : undefined
}

// ─── Throttle: client IP and keys ───────────────────────────────────────

function parseIpv4(value: string): number[] | null {
  const parts = value.split('.')
  if (parts.length !== 4) return null
  const bytes: number[] = []
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null
    const byte = Number(part)
    if (byte > 255) return null
    bytes.push(byte)
  }
  return bytes
}

function parseIpv6(value: string): number[] | null {
  let text = value
  const groups: number[] = []
  // Embedded IPv4 address at the end (`::ffff:192.0.2.1`).
  let tail: number[] = []
  const lastColon = text.lastIndexOf(':')
  if (lastColon >= 0 && text.slice(lastColon + 1).includes('.')) {
    const v4 = parseIpv4(text.slice(lastColon + 1))
    if (!v4) return null
    tail = [(v4[0] << 8) | v4[1], (v4[2] << 8) | v4[3]]
    text = text.slice(0, lastColon + 1)
    // Of a trailing `::` the colon stays, otherwise the rest ends on a single one.
    if (!text.endsWith('::')) text = text.slice(0, -1)
  }
  const halves = text.split('::')
  if (halves.length > 2) return null
  const parseGroups = (part: string): number[] | null => {
    if (part === '') return []
    const result: number[] = []
    for (const group of part.split(':')) {
      if (!/^[0-9a-fA-F]{1,4}$/.test(group)) return null
      result.push(Number.parseInt(group, 16))
    }
    return result
  }
  const head = parseGroups(halves[0])
  const rest = halves.length === 2 ? parseGroups(halves[1]) : []
  if (!head || !rest) return null
  const known = head.length + rest.length + tail.length
  if (halves.length === 2) {
    if (known > 7) return null
    groups.push(...head, ...Array.from({ length: 8 - known }, () => 0), ...rest, ...tail)
  } else {
    if (known !== 8) return null
    groups.push(...head, ...tail)
  }
  return groups
}

/**
 * The client IP in the form the throttle counts it, or `null` if it is not an IP. IPv4 as a whole;
 * IPv6 as its /64 prefix (one subscriber usually has a whole /64, otherwise the throttle would
 * never count per IP); an IPv4 address wrapped in IPv6 (`::ffff:…`) as IPv4.
 */
export function throttleIp(value: string): string | null {
  const text = value.trim()
  if (text.length === 0 || text.length > BRIDGE_LIMITS.ipChars) return null
  const v4 = parseIpv4(text)
  if (v4) return v4.join('.')
  if (!text.includes(':')) return null
  const groups = parseIpv6(text)
  if (!groups) return null
  const mapped = groups.slice(0, 5).every((group) => group === 0) && groups[5] === 0xffff
  if (mapped) {
    return [groups[6] >> 8, groups[6] & 255, groups[7] >> 8, groups[7] & 255].join('.')
  }
  return `${groups
    .slice(0, 4)
    .map((group) => group.toString(16))
    .join(':')}::/64`
}

/**
 * Throttle keys: one per pair of address and client IP, one per IP. Hashed with the edge secret, so
 * that the database holds neither addresses nor IPs (and IPs cannot be computed back). Also counts
 * for addresses that do not exist.
 */
export async function throttleKeys(secret: string, address: string, ip: string) {
  return {
    pairKey: `p:${await sha256Hex(`${secret}\npair\n${ip}\n${address}`)}`,
    ipKey: `i:${await sha256Hex(`${secret}\nip\n${ip}`)}`,
  }
}

// ─── Requests: read the body and check it strictly ──────────────────────

export type RequestProblem = { ok: false; status: 400 | 413; code: 'request' | 'too_large' }
export type Parsed<T> = { ok: true; value: T } | RequestProblem

const MALFORMED: RequestProblem = { ok: false, status: 400, code: 'request' }
const TOO_LARGE: RequestProblem = { ok: false, status: 413, code: 'too_large' }

type Body = Record<string, unknown>

/**
 * Reads the body, at most `maxBytes`; `null` = larger. First the announced length, then chunk by
 * chunk: as soon as the limit is exceeded reading stops (a body without `Content-Length` could
 * otherwise fill the memory of the function before the limit applies).
 */
export async function readLimitedBody(
  request: Request,
  maxBytes: number,
): Promise<RawBytes | null> {
  const declared = Number(request.headers.get('content-length') ?? 0)
  if (Number.isFinite(declared) && declared > maxBytes) return null
  const stream = request.body
  if (!stream) {
    const whole = new Uint8Array(await request.arrayBuffer())
    return whole.byteLength > maxBytes ? null : whole
  }
  const reader = stream.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    total += value.byteLength
    if (total > maxBytes) {
      await reader.cancel().catch(() => undefined)
      return null
    }
    chunks.push(value)
  }
  const bytes = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    bytes.set(chunk, offset)
    offset += chunk.byteLength
  }
  return bytes
}

/**
 * Reads the body as a JSON object, at most `maxBytes`; UTF-8 strictly. Call only AFTER the secret
 * has been checked.
 */
export async function readJsonObject(
  request: Request,
  maxBytes: number = BRIDGE_LIMITS.jsonBytes,
): Promise<Parsed<Body>> {
  const bytes = await readLimitedBody(request, maxBytes)
  if (bytes === null) return TOO_LARGE
  let value: unknown
  try {
    value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
  } catch {
    return MALFORMED
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return MALFORMED
  return { ok: true, value: value as Body }
}

/** Only the named fields: a `mailboxKey` in the body is rejected. */
function onlyKeys(body: Body, allowed: readonly string[]): boolean {
  return Object.keys(body).every((key) => allowed.includes(key))
}

function text(value: unknown, maxChars: number): string | null {
  return typeof value === 'string' && value.length > 0 && value.length <= maxChars ? value : null
}

function integer(value: unknown, min: number, max: number): number | null {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= min && value <= max
    ? value
    : null
}

/** List of UIDs (1 to 2^32 − 1), at most `BRIDGE_LIMITS.uids`. */
function uidList(value: unknown): number[] | null {
  if (!Array.isArray(value) || value.length > BRIDGE_LIMITS.uids) return null
  const uids: number[] = []
  for (const entry of value) {
    const uid = integer(entry, 1, MAX_UID)
    if (uid === null) return null
    uids.push(uid)
  }
  return uids
}

/** List of flags; missing or `null` counts as empty. */
function flagList(value: unknown): string[] | null {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value) || value.length > BRIDGE_LIMITS.flags) return null
  const flags: string[] = []
  for (const entry of value) {
    const flag = text(entry, BRIDGE_LIMITS.flagChars)
    if (flag === null) return null
    flags.push(flag)
  }
  return flags
}

export type LoginRequest = { address: string; password: string; ip: string; bridge: number }

/** `/login`: normalized address, password, client IP in the form of the throttle. */
export function parseLogin(body: Body): Parsed<LoginRequest> {
  if (!onlyKeys(body, ['address', 'password', 'clientIp', 'bridge'])) return MALFORMED
  // Version of the bridge; a bridge that does not send it predates folders.
  const bridge = body.bridge === undefined ? 1 : integer(body.bridge, 1, 1000)
  if (bridge === null) return MALFORMED
  const address = text(body.address, BRIDGE_LIMITS.addressChars)
  const password = text(body.password, BRIDGE_LIMITS.passwordChars)
  const clientIp = text(body.clientIp, BRIDGE_LIMITS.ipChars)
  const ip = clientIp === null ? null : throttleIp(clientIp)
  if (address === null || password === null || ip === null) return MALFORMED
  const normalized = normalizeAddress(address)
  if (normalized.length === 0) return MALFORMED
  return { ok: true, value: { address: normalized, password, ip, bridge } }
}

/** `/folders` and other routes without input: exactly `{}`. */
export function parseEmpty(body: Body): Parsed<Record<string, never>> {
  return onlyKeys(body, []) ? { ok: true, value: {} } : MALFORMED
}

export type EntriesRequest = { folder: string; afterUid?: number; limit: number }

export function parseEntries(body: Body): Parsed<EntriesRequest> {
  if (!onlyKeys(body, ['folder', 'afterUid', 'limit'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const limit = integer(body.limit, 1, BRIDGE_LIMITS.rows)
  const absent = body.afterUid === undefined || body.afterUid === null
  const afterUid = absent ? undefined : integer(body.afterUid, 0, MAX_UID)
  if (folder === null || limit === null || afterUid === null) return MALFORMED
  return { ok: true, value: { folder, limit, ...(afterUid !== undefined ? { afterUid } : {}) } }
}

export type ChangesRequest = { folder: string; sinceModseq: number; limit: number }

export function parseChanges(body: Body): Parsed<ChangesRequest> {
  if (!onlyKeys(body, ['folder', 'sinceModseq', 'limit'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const sinceModseq = integer(body.sinceModseq, 0, Number.MAX_SAFE_INTEGER)
  const limit = integer(body.limit, 1, BRIDGE_LIMITS.rows)
  if (folder === null || sinceModseq === null || limit === null) return MALFORMED
  return { ok: true, value: { folder, sinceModseq, limit } }
}

export type RawRequest = { folder: string; uid: number; uidValidity: number }

/** `/raw`: the UID together with the UIDVALIDITY the bridge learned it under. */
export function parseRaw(body: Body): Parsed<RawRequest> {
  if (!onlyKeys(body, ['folder', 'uid', 'uidValidity'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const uid = integer(body.uid, 1, MAX_UID)
  const uidValidity = integer(body.uidValidity, 1, MAX_UID)
  if (folder === null || uid === null || uidValidity === null) return MALFORMED
  return { ok: true, value: { folder, uid, uidValidity } }
}

export type StoreRequest = { folder: string; uids: number[]; add: string[]; remove: string[] }

export function parseStore(body: Body): Parsed<StoreRequest> {
  if (!onlyKeys(body, ['folder', 'uids', 'add', 'remove'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const uids = uidList(body.uids)
  const add = flagList(body.add)
  const remove = flagList(body.remove)
  if (folder === null || uids === null || add === null || remove === null) return MALFORMED
  return { ok: true, value: { folder, uids, add, remove } }
}

/** Does a list of flags name `\Seen` (in any spelling the component reads)? */
export function namesSeenFlag(flags: readonly string[]): boolean {
  return flags.some((flag) => flag.toLowerCase() === '\\seen')
}

/** Splits a list into batches of a fixed size; an empty list gives exactly one empty batch. */
export function inBatches<T>(items: readonly T[], size: number): T[][] {
  const batches: T[][] = []
  for (let index = 0; index < items.length; index += size) {
    batches.push(items.slice(index, index + size))
  }
  return batches.length > 0 ? batches : [[]]
}

export type ExpungeRequest = { folder: string; uids: number[] }

export function parseExpunge(body: Body): Parsed<ExpungeRequest> {
  if (!onlyKeys(body, ['folder', 'uids'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const uids = uidList(body.uids)
  if (folder === null || uids === null) return MALFORMED
  return { ok: true, value: { folder, uids } }
}

export type AppendRequest = { folder: string; messageId: string; digest: string }

/**
 * `/append`: the Message-ID normalized (without angle brackets). `digest` (SHA-256 of the raw
 * bytes, hex) is only checked for its shape.
 */
export function parseAppend(body: Body): Parsed<AppendRequest> {
  if (!onlyKeys(body, ['folder', 'messageId', 'digest'])) return MALFORMED
  const folder = text(body.folder, BRIDGE_LIMITS.folderChars)
  const rawMessageId = text(body.messageId, BRIDGE_LIMITS.messageIdChars)
  const messageId = rawMessageId === null ? undefined : normalizeMessageId(rawMessageId)
  const digest = typeof body.digest === 'string' ? body.digest.toLowerCase() : ''
  if (folder === null || messageId === undefined || !/^[0-9a-f]{64}$/.test(digest)) {
    return MALFORMED
  }
  return { ok: true, value: { folder, messageId, digest } }
}

/**
 * `/seq`: at most `BRIDGE_LIMITS.sessions` tokens. An entry that cannot be a token stays in its
 * place (empty) and is answered as invalid, not as an error.
 */
export function parseSeq(body: Body): Parsed<{ sessions: string[] }> {
  if (!onlyKeys(body, ['sessions'])) return MALFORMED
  const list = body.sessions
  if (!Array.isArray(list) || list.length > BRIDGE_LIMITS.sessions) return MALFORMED
  const sessions: string[] = []
  for (const entry of list) {
    if (typeof entry !== 'string') return MALFORMED
    sessions.push(isTokenShape(entry) ? entry : '')
  }
  return { ok: true, value: { sessions } }
}

export type SubmitEnvelope = { from: string; to: string[] }

/**
 * Envelope of a submission (`/submit`) from the request headers: `X-Envelope-From` (one address)
 * and `X-Envelope-To` (JSON list of addresses), lowercased. Only the shape is checked here; a
 * missing header or a list that is not JSON is a bug of the bridge (`400 request`).
 */
export function parseEnvelope(from: string | null, to: string | null): Parsed<SubmitEnvelope> {
  if (from === null || from.length > BRIDGE_LIMITS.addressChars || to === null) return MALFORMED
  let list: unknown
  try {
    list = JSON.parse(to)
  } catch {
    return MALFORMED
  }
  if (!Array.isArray(list) || list.length === 0 || list.length > BRIDGE_LIMITS.envelopeRecipients) {
    return MALFORMED
  }
  const recipients: string[] = []
  for (const entry of list) {
    if (typeof entry !== 'string' || entry.length > BRIDGE_LIMITS.addressChars) return MALFORMED
    recipients.push(normalizeAddress(entry))
  }
  return { ok: true, value: { from: normalizeAddress(from), to: recipients } }
}

// ─── Responses ──────────────────────────────────────────────────────────

const JSON_HEADERS = { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }

export function bridgeJson(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { ...JSON_HEADERS, ...headers } })
}

/** A short text per error code, without addresses or content (it ends up in the bridge's log). */
const ERROR_TEXT = {
  auth: 'Sign-in failed.',
  throttled: 'Too many failed attempts, try again later.',
  preparing: 'The mailbox is being prepared.',
  session: 'Session invalid or expired.',
  folder: 'Folder unknown or not allowed here.',
  gone: 'Message no longer exists.',
  uidvalidity: 'The folder was renumbered, read it again.',
  resync: 'State too old, read the folder again.',
  unknown: 'Message unknown.',
  request: 'Invalid request.',
  too_large: 'Request too large.',
  internal: 'Internal error.',
} as const

export type BridgeErrorCode = keyof typeof ERROR_TEXT

/** Error shape of the contract: `{"error":"<code>","message":"<short text>"}` plus extra fields. */
export function bridgeError(
  status: number,
  code: BridgeErrorCode,
  extra: Record<string, unknown> = {},
  headers: Record<string, string> = {},
) {
  return bridgeJson({ error: code, ...extra, message: ERROR_TEXT[code] }, status, headers)
}

/**
 * Why a submission is refused. The bridge passes `smtpCode` and the message on to the mail client
 * and replaces everything but printable ASCII with `?`, so the texts are plain ASCII.
 */
export const SUBMIT_REJECTIONS = {
  sender: { status: 422, smtpCode: 550, message: 'The sender must be the address of this mailbox.' },
  recipients: {
    status: 422,
    smtpCode: 550,
    message: 'The recipients of the envelope do not match To and Cc.',
  },
  content: { status: 422, smtpCode: 554, message: 'The message cannot be read.' },
  size: { status: 413, smtpCode: 552, message: 'The message is too large.' },
  conflict: {
    status: 409,
    smtpCode: 550,
    message: 'A different message with this Message-ID was already submitted.',
  },
  busy: { status: 409, smtpCode: 451, message: 'This message is still being sent.' },
  limit: { status: 429, smtpCode: 451, message: 'Too many messages, try again later.' },
  rejected: { status: 422, smtpCode: 550, message: 'The message was rejected.' },
  unavailable: { status: 503, smtpCode: 451, message: 'Sending is unavailable, try again later.' },
} as const

export type SubmitRejection = keyof typeof SUBMIT_REJECTIONS

/** Everything but printable ASCII becomes `?`, at most 200 characters. */
export function asciiMessage(value: string): string {
  return value.replace(/[^\x20-\x7e]/g, '?').slice(0, 200)
}

/** Refusal of a submission: `{"error","smtpCode","message"}` with the status of the contract. */
export function bridgeSubmitRejection(reason: SubmitRejection, message?: string) {
  const rejection = SUBMIT_REJECTIONS[reason]
  return bridgeJson(
    {
      error: reason,
      smtpCode: rejection.smtpCode,
      message: asciiMessage(message ?? rejection.message),
    },
    rejection.status,
  )
}

/** Without or with a wrong secret: 401, empty body. */
export function bridgeUnauthorized() {
  return new Response(null, { status: 401, headers: { 'Cache-Control': 'no-store' } })
}
