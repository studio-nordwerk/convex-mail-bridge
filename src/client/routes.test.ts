// The client layer with a minimal app that exists only in this file: one mailbox, callbacks that
// answer from constants, and whatever route options a test needs. The example app
// (example/convex) covers the routes with real tables; here are the cases that need a different
// set-up than the example has: no `submit`, a location that is no storage id, the calls of
// `login`.

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { convexTest } from 'convex-test'
import { anyApi, componentsGeneric, httpRouter, type GenericDataModel } from 'convex/server'
import type { ComponentApi } from '../component/_generated/component.js'
import { register } from '../test.js'
import {
  MailBridge,
  type EdgeCallbacks,
  type EdgeFunctionReferences,
  type RouteOptions,
  type Submission,
} from './index.js'

const SECRET = 'a-shared-secret-with-at-least-32-chars'
const ADDRESS = 'ada@example.com'
const MAILBOX = { mailboxKey: 'mailbox-1', ownerKey: 'user-ada' }
const IP = '192.0.2.10'
const functions = anyApi.mailBridge as unknown as EdgeFunctionReferences

type Callbacks = EdgeCallbacks<GenericDataModel>
type Routes = Omit<RouteOptions<GenericDataModel>, 'functions'>

const callbacks: Callbacks = {
  mailboxByAddress: async (_ctx, address) => (address === ADDRESS ? MAILBOX : null),
  mailboxOwner: async (_ctx, mailboxKey) =>
    mailboxKey === MAILBOX.mailboxKey ? MAILBOX.ownerKey : null,
  rawLocation: async () => null,
}

/** An app made of the edge functions and the routes, with one enabled mailbox and one device. */
async function miniApp(overrides: Partial<Callbacks> = {}, routes: Routes = {}) {
  const component = componentsGeneric().mailBridge as unknown as ComponentApi
  const bridge = new MailBridge(component, { secret: SECRET })
  const http = httpRouter()
  bridge.registerRoutes(http, { functions, ...routes })
  const edge = bridge.edge({ ...callbacks, ...overrides })
  const t = convexTest(undefined, {
    '/app/_generated/api.js': async () => ({}),
    '/app/mailBridge.ts': async () => edge,
    '/app/http.ts': async () => ({ default: http }),
  })
  register(t)
  const { password, secretHash } = await bridge.newDevicePassword()
  await t.run(async (ctx) => {
    await bridge.enable(ctx, MAILBOX)
    await bridge.addDevice(ctx, { ...MAILBOX, label: 'Laptop', secretHash })
  })
  const post = async (route: string, body: BodyInit, headers: Record<string, string> = {}) =>
    await t.fetch(`/bridge/v1/${route}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${SECRET}`, ...headers },
      body,
    })
  const login = await post('login', JSON.stringify({ address: ADDRESS, password, clientIp: IP }))
  expect(login.status).toBe(200)
  const { session } = (await login.json()) as { session: string }
  const submit = async (token: string = session) =>
    await post('submit', 'Subject: Hello\r\n\r\nHello.\r\n', {
      'Content-Type': 'message/rfc822',
      'X-Bridge-Session': token,
      'X-Envelope-From': ADDRESS,
      'X-Envelope-To': JSON.stringify(['grace@example.com']),
    })
  return { t, bridge, post, session, submit }
}

/** A token of the right shape that no session has. */
const STRANGER = 'A'.repeat(43)

describe('routes of the client layer', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-15T10:00:00Z'))
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  describe('/submit without a way to send', () => {
    test('answers unavailable and does not count an attempt', async () => {
      const allowSubmit = vi.fn(async () => true)
      const app = await miniApp({ allowSubmit })
      const response = await app.submit()
      expect(response.status).toBe(503)
      expect(await response.json()).toMatchObject({ error: 'unavailable', smtpCode: 451 })
      expect(allowSubmit).not.toHaveBeenCalled()
    })

    test('still ends a connection whose session is invalid', async () => {
      const allowSubmit = vi.fn(async () => true)
      const app = await miniApp({ allowSubmit })
      const response = await app.submit(STRANGER)
      expect(response.status).toBe(401)
      expect(await response.json()).toEqual({ error: 'session', message: 'Session invalid or expired.' })
      expect(allowSubmit).not.toHaveBeenCalled()
    })

    test('with a way to send, every attempt is counted once and a refusal reads no body', async () => {
      let allowed = true
      const allowSubmit = vi.fn(async () => allowed)
      const submit = vi.fn(async () => ({ status: 'unknown' as const }))
      const app = await miniApp({ allowSubmit }, { submit })
      expect((await app.submit()).status).toBe(200)
      expect(allowSubmit).toHaveBeenCalledTimes(1)
      allowed = false
      const refused = await app.submit()
      expect(refused.status).toBe(429)
      expect(await refused.json()).toMatchObject({ error: 'limit', smtpCode: 451 })
      expect(allowSubmit).toHaveBeenCalledTimes(2)
      expect(submit).toHaveBeenCalledTimes(1)
    })
  })

  describe('/submit and the session at the time of storing', () => {
    test('the submission carries a handle that the session function resolves', async () => {
      const seen: Array<{ submission: Submission; open: unknown }> = []
      const app = await miniApp(
        {},
        {
          submit: async (ctx, submission) => {
            const open = await ctx.runQuery(functions.session, {
              tokenHash: submission.tokenHash,
              now: Date.now(),
            })
            seen.push({ submission, open })
            return { status: 'unknown' }
          },
        },
      )
      expect((await app.submit()).status).toBe(200)
      expect(seen).toHaveLength(1)
      expect(seen[0].open).toEqual({ ...MAILBOX, userKey: MAILBOX.ownerKey })
      // The handle is the hash the ledger stores, not the token the bridge holds.
      expect(seen[0].submission.tokenHash).toMatch(/^[0-9a-f]{64}$/)
      expect(seen[0].submission.tokenHash).not.toBe(app.session)
    })

    test('once the device password is revoked the session function answers null, and counts nothing', async () => {
      const allowSubmit = vi.fn(async () => true)
      const opened: unknown[] = []
      let revoke = async () => {}
      const app = await miniApp(
        { allowSubmit },
        {
          submit: async (ctx, submission) => {
            const args = { tokenHash: submission.tokenHash, now: Date.now() }
            opened.push(await ctx.runQuery(functions.session, args))
            await revoke()
            opened.push(await ctx.runQuery(functions.session, args))
            return { status: 'session' }
          },
        },
      )
      revoke = async () => {
        await app.t.run(async (ctx) => await app.bridge.revokeAll(ctx, MAILBOX.mailboxKey))
      }
      const response = await app.submit()
      expect(opened).toEqual([{ ...MAILBOX, userKey: MAILBOX.ownerKey }, null])
      // What `submit` reports as an ended session is the contract's answer for one.
      expect(response.status).toBe(401)
      expect(await response.json()).toEqual({ error: 'session', message: 'Session invalid or expired.' })
      expect(allowSubmit).toHaveBeenCalledTimes(1)
    })
  })

  describe('/raw with the default loader', () => {
    const LOCATION = 'not-a-storage-id'

    async function withMessage(routes: Routes = {}) {
      const app = await miniApp({ rawLocation: async () => LOCATION }, routes)
      await app.t.run(async (ctx) => {
        await app.bridge.place(ctx, {
          mailboxKey: MAILBOX.mailboxKey,
          messageKey: 'message-1',
          folder: 'INBOX',
          size: 10,
          internalDate: Date.now(),
          seen: false,
        })
      })
      const [inbox] = await app.t.run(async (ctx) => await app.bridge.folders(ctx, MAILBOX.mailboxKey))
      const raw = async () =>
        await app.post('raw', JSON.stringify({ folder: 'INBOX', uid: 1, uidValidity: inbox.uidValidity }), {
          'X-Bridge-Session': app.session,
        })
      return { ...app, raw }
    }

    test('a location that is no storage id is a missing message, with a warning that names nothing', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
      const error = vi.spyOn(console, 'error').mockImplementation(() => {})
      const app = await withMessage()
      const response = await app.raw()
      expect(response.status).toBe(404)
      expect(await response.json()).toEqual({ error: 'gone', message: 'Message no longer exists.' })
      expect(error).not.toHaveBeenCalled()
      expect(warn).toHaveBeenCalledTimes(1)
      const line = String(warn.mock.calls[0][0])
      expect(line).toMatch(/^\[mail-bridge\] raw: location is not readable/)
      expect(line).not.toContain(LOCATION)
      expect(line).not.toContain(MAILBOX.mailboxKey)
    })

    test('a loader of the app decides itself: its error stays an internal error', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      const app = await withMessage({
        loadRaw: async () => {
          throw new Error('storage of the app is down')
        },
      })
      const response = await app.raw()
      expect(response.status).toBe(500)
      expect(await response.json()).toEqual({ error: 'internal', message: 'Internal error.' })
    })
  })

  // The handler of `login` with a context that only records: which functions of the component
  // are called, in which order. An address the app does not know must be indistinguishable from
  // one it knows by the work it causes.
  describe('/login makes the same component calls for known and unknown addresses', () => {
    type Case = { address: string; holder: { userKey: string | null } | null; member?: boolean }
    const CASES: Record<string, Case> = {
      'unknown address': { address: 'nobody@example.com', holder: null },
      'known address, wrong password': { address: ADDRESS, holder: null },
      'known address, device of a removed member': { address: ADDRESS, holder: { userKey: 'user-grace' } },
      'known address, valid device of the owner': { address: ADDRESS, holder: { userKey: null } },
      'known address, valid device of a member': {
        address: ADDRESS,
        holder: { userKey: 'user-grace' },
        member: true,
      },
    }
    const HASH = 'a'.repeat(64)

    async function callsOf(input: Case, bridge: number | undefined) {
      const refs = {
        devices: { holder: 'devices.holder' },
        folders: { list: 'folders.list' },
        sessions: { login: 'sessions.login' },
      }
      const calls: Array<{ call: string; mailboxKey: unknown }> = []
      const ctx = {
        runQuery: async (ref: string, args: { mailboxKey: string }) => {
          calls.push({ call: `query ${ref}`, mailboxKey: args.mailboxKey })
          if (ref === refs.devices.holder) return input.holder
          if (ref === refs.folders.list) return []
          throw new Error(`unexpected query ${ref}`)
        },
        runMutation: async (ref: string, args: { mailboxKey: string | null }) => {
          calls.push({ call: `mutation ${ref}`, mailboxKey: args.mailboxKey })
          return args.mailboxKey === null
            ? { kind: 'auth' }
            : { kind: 'ok', expiresAt: Date.now() + 1000 }
        },
      }
      const edge = new MailBridge(refs as unknown as ComponentApi, { secret: SECRET }).edge({
        ...callbacks,
        allowMember: async () => input.member === true,
      })
      const handler = (edge.login as unknown as { _handler: (ctx: unknown, args: unknown) => Promise<{ kind: string }> })
        ._handler
      const result = await handler(ctx, {
        address: input.address,
        secretHash: HASH,
        tokenHash: HASH,
        pairKey: `p:${HASH}`,
        ipKey: `i:${HASH}`,
        ...(bridge !== undefined ? { bridge } : {}),
      })
      return { calls, result }
    }

    test.each([
      [undefined, ['query devices.holder', 'query folders.list', 'mutation sessions.login']],
      [1, ['query devices.holder', 'query folders.list', 'mutation sessions.login']],
      [2, ['query devices.holder', 'mutation sessions.login']],
    ])('bridge version %s', async (bridge, expected) => {
      for (const [name, input] of Object.entries(CASES)) {
        const { calls } = await callsOf(input, bridge)
        expect(calls.map((entry) => entry.call), name).toEqual(expected)
      }
    })

    test('without a mailbox the component is asked with an empty key and told there is none', async () => {
      for (const name of ['unknown address', 'known address, device of a removed member']) {
        const { calls, result } = await callsOf(CASES[name], 1)
        expect(calls.map((entry) => entry.mailboxKey), name).toEqual(
          name === 'unknown address' ? ['', '', null] : [MAILBOX.mailboxKey, '', null],
        )
        expect(result, name).toEqual({ kind: 'auth' })
      }
      const valid = await callsOf(CASES['known address, valid device of the owner'], 1)
      expect(valid.calls.map((entry) => entry.mailboxKey)).toEqual(Array(3).fill(MAILBOX.mailboxKey))
      expect(valid.result).toMatchObject({ kind: 'ok', mailbox: MAILBOX.mailboxKey })
    })
  })
})
