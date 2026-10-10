import { defineApp } from 'convex/server'
import mailBridge from '@nordwerk/convex-mail-bridge/convex.config.js'

const app = defineApp()
app.use(mailBridge)

export default app
