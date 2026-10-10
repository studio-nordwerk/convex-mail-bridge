// Configuration profile for Apple Mail (iPhone, iPad, Mac): adds an IMAP account with server,
// ports and user name. Pure generation, usable in two ways:
//
// - Without a password: the device asks for it once when the profile is installed, for incoming
//   and outgoing mail together (`OutgoingPasswordSameAsIncomingPassword`).
// - With the password of a device you have just created in `IncomingPassword`. The password is
//   then only in the response, stored nowhere. Apple's schema advises to use passwords only in
//   encrypted profiles, and encrypting needs MDM. If you do this, hand the profile out through a
//   link that works once and only briefly, and use a password per device that can be revoked.
//
// Only keys from Apple's schema (github.com/apple/device-management):
// `mdm/profiles/com.apple.mail.managed.yaml` for the account, `CommonPayloadKeys.yaml` and
// `TopLevel.yaml` for the frame. The schema has no key for where drafts or the trash are kept;
// users set that in the mail client.

export type MailProfileInput = {
  /** Address of the mailbox: also the user name for incoming and outgoing mail. */
  address: string
  /** Name of the person as it appears as the sender (optional). */
  displayName?: string
  /** Host name of the bridge, for example `mail.example.com`. */
  host: string
  imapPort: number
  smtpPort: number
  /** New for every profile (`crypto.randomUUID()`): one id for the profile, one for the account. */
  uuids: { profile: string; account: string }
  /** Device password. If missing, the device asks for it when the profile is installed. */
  password?: string
  /** Texts shown in the settings of the device. */
  texts: { profileName: string; profileDescription: string; accountName: string }
}

type PlistValue = string | number | boolean | PlistValue[] | { [key: string]: PlistValue }

/** Characters XML 1.0 allows (no control characters except tab, line feed, carriage return). */
function isXmlChar(code: number): boolean {
  return (
    code === 0x9 ||
    code === 0xa ||
    code === 0xd ||
    (code >= 0x20 && code <= 0xd7ff) ||
    (code >= 0xe000 && code <= 0xfffd) ||
    (code >= 0x10000 && code <= 0x10ffff)
  )
}

/** Text for XML: characters that are not allowed are dropped, special characters are escaped. */
export function escapeXml(value: string): string {
  let output = ''
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0
    if (!isXmlChar(code)) continue
    if (char === '&') output += '&amp;'
    else if (char === '<') output += '&lt;'
    else if (char === '>') output += '&gt;'
    else if (char === '"') output += '&quot;'
    else if (char === "'") output += '&apos;'
    else output += char
  }
  return output
}

function plist(value: PlistValue, indent: string): string {
  if (typeof value === 'string') return `${indent}<string>${escapeXml(value)}</string>`
  if (typeof value === 'number') return `${indent}<integer>${Math.trunc(value)}</integer>`
  if (typeof value === 'boolean') return `${indent}<${value ? 'true' : 'false'}/>`
  if (Array.isArray(value)) {
    const items = value.map((item) => plist(item, `${indent}\t`))
    return [`${indent}<array>`, ...items, `${indent}</array>`].join('\n')
  }
  const lines = [`${indent}<dict>`]
  for (const key of Object.keys(value).sort()) {
    lines.push(`${indent}\t<key>${escapeXml(key)}</key>`, plist(value[key], `${indent}\t`))
  }
  lines.push(`${indent}</dict>`)
  return lines.join('\n')
}

/** `mail.example.com` → `com.example.mail`: prefix of the identifiers in the profile. */
function reverseHost(host: string): string {
  return host.toLowerCase().split('.').filter(Boolean).reverse().join('.')
}

/** The profile as a property list (XML), ready to be saved as `.mobileconfig`. */
export function buildMailProfile(input: MailProfileInput): string {
  const namespace = `${reverseHost(input.host)}.mailbridge`
  const account: { [key: string]: PlistValue } = {
    EmailAccountDescription: input.texts.accountName,
    EmailAccountType: 'EmailTypeIMAP',
    EmailAddress: input.address,
    IncomingMailServerAuthentication: 'EmailAuthPassword',
    IncomingMailServerHostName: input.host,
    IncomingMailServerPortNumber: input.imapPort,
    IncomingMailServerUseSSL: true,
    IncomingMailServerUsername: input.address,
    OutgoingMailServerAuthentication: 'EmailAuthPassword',
    OutgoingMailServerHostName: input.host,
    OutgoingMailServerPortNumber: input.smtpPort,
    OutgoingMailServerUseSSL: true,
    OutgoingMailServerUsername: input.address,
    // One password for both directions: the device asks only once.
    OutgoingPasswordSameAsIncomingPassword: true,
    PayloadDisplayName: input.texts.accountName,
    PayloadIdentifier: `${namespace}.account.${input.uuids.account}`,
    PayloadType: 'com.apple.mail.managed',
    PayloadUUID: input.uuids.account,
    PayloadVersion: 1,
  }
  const displayName = input.displayName?.trim()
  if (displayName) account.EmailAccountName = displayName
  if (input.password !== undefined && input.password.length > 0) {
    // A character XML does not know would be dropped when escaping: the device would silently
    // get a different password. Rather no profile at all.
    for (const char of input.password) {
      if (!isXmlChar(char.codePointAt(0) ?? 0)) throw new Error('mobileconfig: invalid password')
    }
    account.IncomingPassword = input.password
  }
  const profile: PlistValue = {
    PayloadContent: [account],
    PayloadDescription: input.texts.profileDescription,
    PayloadDisplayName: input.texts.profileName,
    PayloadIdentifier: `${namespace}.profile.${input.uuids.profile}`,
    PayloadType: 'Configuration',
    PayloadUUID: input.uuids.profile,
    PayloadVersion: 1,
  }
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    plist(profile, ''),
    '</plist>',
    '',
  ].join('\n')
}

/** File name for saving: from the address, harmless characters only. */
export function mailProfileFilename(address: string): string {
  const base = address
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/^[-.]+|[-.]+$/g, '')
  return `${base || 'mailbox'}.mobileconfig`
}

/** Media type under which iPhone and Mac recognize a configuration profile. */
export const MAIL_PROFILE_MIME = 'application/x-apple-aspen-config'
