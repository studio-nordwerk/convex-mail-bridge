import { cronJobs } from 'convex/server'
import { internal } from './_generated/api.js'

const crons = cronJobs()

// Expired sessions, old throttle windows, revoked device passwords and tombstones.
crons.interval('clean up mail bridge', { hours: 1 }, internal.mailBridge.cleanup, {})

export default crons
