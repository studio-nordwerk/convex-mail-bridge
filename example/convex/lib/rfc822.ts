// Just enough RFC 822 for the example. A real app parses mail with a MIME library.

export type DemoMessage = {
  from: string
  to: string
  subject: string
  text: string
  messageId: string
  date: number
}

/** A plain text message with CRLF line ends, as bytes. */
export function buildMessage(message: DemoMessage): Uint8Array<ArrayBuffer> {
  const lines = [
    `From: ${message.from}`,
    `To: ${message.to}`,
    `Subject: ${message.subject}`,
    `Date: ${new Date(message.date).toUTCString()}`,
    `Message-ID: <${message.messageId}>`,
    'MIME-Version: 1.0',
    'Content-Type: text/plain; charset=utf-8',
    'Content-Transfer-Encoding: 8bit',
    '',
    ...message.text.split('\n'),
    '',
  ]
  return new TextEncoder().encode(lines.join('\r\n'))
}

/** The first value of a header, unfolded, or `null`. Looks at the header block only. */
export function headerOf(raw: Uint8Array, name: string): string | null {
  const text = new TextDecoder('utf-8').decode(raw.subarray(0, 64 * 1024))
  const end = text.search(/\r?\n\r?\n/)
  const head = (end < 0 ? text : text.slice(0, end)).replace(/\r?\n[ \t]+/g, ' ')
  for (const line of head.split(/\r?\n/)) {
    const colon = line.indexOf(':')
    if (colon > 0 && line.slice(0, colon).trim().toLowerCase() === name.toLowerCase()) {
      return line.slice(colon + 1).trim()
    }
  }
  return null
}

/** `Ada <ada@example.com>` → `ada@example.com`. */
export function addressOf(header: string | null): string | null {
  if (!header) return null
  const match = /<([^<>\s]+@[^<>\s]+)>/.exec(header) ?? /([^\s<>,;]+@[^\s<>,;]+)/.exec(header)
  return match ? match[1].toLowerCase() : null
}
