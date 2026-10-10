import { describe, expect, test } from 'vitest'
import { MAIL_PROFILE_MIME, buildMailProfile, escapeXml, mailProfileFilename } from './mobileconfig.js'

const input = {
  address: 'ada@example.com',
  displayName: 'Ada & Co <Lovelace>',
  host: 'mail.example.com',
  imapPort: 993,
  smtpPort: 465,
  uuids: {
    profile: '11111111-1111-4111-8111-111111111111',
    account: '22222222-2222-4222-8222-222222222222',
  },
  texts: { profileName: 'Mail', profileDescription: 'IMAP account', accountName: 'ada@example.com' },
}

function valueOf(profile: string, key: string) {
  const match = new RegExp(`<key>${key}</key>\\s*<(?:string|integer)>([^<]*)<`).exec(profile)
  return match ? match[1] : null
}

describe('mail profile', () => {
  test('carries server, ports and user name, and no password unless given', () => {
    const profile = buildMailProfile(input)
    expect(profile.startsWith('<?xml version="1.0" encoding="UTF-8"?>')).toBe(true)
    expect(valueOf(profile, 'IncomingMailServerHostName')).toBe('mail.example.com')
    expect(valueOf(profile, 'IncomingMailServerPortNumber')).toBe('993')
    expect(valueOf(profile, 'OutgoingMailServerPortNumber')).toBe('465')
    expect(valueOf(profile, 'IncomingMailServerUsername')).toBe('ada@example.com')
    expect(valueOf(profile, 'EmailAccountName')).toBe('Ada &amp; Co &lt;Lovelace&gt;')
    expect(valueOf(profile, 'PayloadIdentifier')).toContain('com.example.mail.mailbridge.')
    expect(profile).toContain('<key>OutgoingPasswordSameAsIncomingPassword</key>\n\t\t\t<true/>')
    expect(profile).not.toContain('<key>IncomingPassword</key>')
  })

  test('with a password the profile contains it, and refuses one XML cannot carry', () => {
    const profile = buildMailProfile({ ...input, password: 'mb_abc234' })
    expect(valueOf(profile, 'IncomingPassword')).toBe('mb_abc234')
    expect(() => buildMailProfile({ ...input, password: 'bad\u0001' })).toThrow('mobileconfig: invalid password')
  })

  test('the same input gives the same bytes', () => {
    expect(buildMailProfile(input)).toBe(buildMailProfile(input))
  })

  test('helpers', () => {
    expect(escapeXml('a\u0000<b>&"\'')).toBe('a&lt;b&gt;&amp;&quot;&apos;')
    expect(mailProfileFilename('Ada+Test@Example.com')).toBe('ada-test-example.com.mobileconfig')
    expect(mailProfileFilename('@@')).toBe('mailbox.mobileconfig')
    expect(MAIL_PROFILE_MIME).toBe('application/x-apple-aspen-config')
  })
})
