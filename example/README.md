# Example app

A small mail app that uses the component: mailboxes, messages, members of shared mailboxes. All
data is invented.

| File | What it shows |
| --- | --- |
| `convex/schema.ts` | the app's own tables; the component knows none of them |
| `convex/mailBridge.ts` | the `MailBridge` instance and the callbacks: the file you write for your app |
| `convex/mail.ts` | storing, reading and deleting messages together with the ledger; device passwords |
| `convex/http.ts` | mounting `/bridge/v1/*` and a `submit` that records instead of sending |
| `convex/crons.ts` | hourly housekeeping |
| `convex/bridge.test.ts` | every route of the edge contract, driven through `t.fetch` |

## Tests

From the repository root:

```sh
npm run check
```

## Trying it with a real mail client

You need a Convex deployment (a cloud project or a local, self-hosted backend) and Docker.

```sh
npx convex dev                       # in the repository root; deploys example/convex
npx convex env set MAIL_BRIDGE_SECRET "$(openssl rand -hex 32)"
npx convex run mail:seed             # prints an address and a device password, once
```

Start the bridge against the deployment, without TLS, on localhost only:

```sh
cd bridge
docker build -t mail-bridge .
docker run --rm -p 127.0.0.1:1143:1143 -p 127.0.0.1:1465:1465 \
  -e CONVEX_SITE_URL=https://<deployment>.convex.site \
  -e MAIL_BRIDGE_SECRET=<the same secret> \
  -e PROXY_PROTOCOL=off \
  mail-bridge
```

Then add an IMAP account in your mail client: user name `ada@example.com`, the device password,
server `127.0.0.1`, IMAP port 1143, SMTP port 1465, TLS off for both. Send yourself a message:

```sh
npx convex run mail:receive '{"mailboxId":"<id>","from":"grace@example.com","subject":"Hi","text":"Hello"}'
```

It should appear within a few seconds without fetching.

The maintainers have run `mail:seed` and `mail:receive` like this against a local, self-hosted
Convex backend and checked the bridge with the probe and `mbsync`, not with a real mail client;
the `docker run` line above with the production image was not part of that run. See
[docs/limits.md](../docs/limits.md).
