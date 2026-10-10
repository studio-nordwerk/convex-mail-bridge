import { defineComponent } from 'convex/server'

// The ledger for mail clients: IMAP UIDs and counters per mailbox and folder, device passwords,
// session tokens and the throttle for failed sign-ins. See docs/architecture.md.
const component = defineComponent('mailBridge')

export default component
