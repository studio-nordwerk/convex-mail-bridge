# Running the bridge

This page covers deploying the bridge (the Go service in `bridge/`) and operating it. For what
the bridge does and its settings, see [`../bridge/README.md`](../bridge/README.md); for the
interface to the Convex app, [`edge-contract.md`](edge-contract.md); for what is not supported or
not verified, [`limits.md`](limits.md).

## Requirements

- **A TLS-terminating TCP proxy** in front of the bridge. Mail clients connect to ports 993
  (IMAP) and 465 (SMTP submission), both with TLS from the first byte. The proxy terminates TLS
  and forwards the plain bytes to the bridge (default ports 1143 and 1465), with a PROXY protocol
  header (v1 or v2) in front of each connection. The header is how the bridge learns the client
  address, which the edge needs to throttle failed logins.
- The bridge speaks plaintext behind the proxy and offers no STARTTLS. Plaintext from the
  internet must never reach it: device passwords would travel unprotected. Only the proxy may be
  able to reach the mail ports.
- **Two settings.** `CONVEX_SITE_URL` (`https://<deployment>.convex.site`) and
  `MAIL_BRIDGE_SECRET`. The secret must be the same value as `MAIL_BRIDGE_SECRET` in the
  environment of your Convex deployment, and at least 32 characters long; with a shorter secret
  the edge is off and answers only `401`. Generate one with:

  ```sh
  openssl rand -hex 32
  ```

  Set it in the Convex deployment, for example:

  ```sh
  npx convex env set MAIL_BRIDGE_SECRET <secret> --prod
  ```

  Keep the secret out of version control, shell history and chat.
- **A host name and a certificate**, for example `mail.example.com`, resolving to the proxy.

## Fly.io

This walk-through uses the app name `my-mail-bridge` and the host `mail.example.com`. The example
configuration is `bridge/deploy/fly/fly.toml`. Copy it next to the Dockerfile
(`cp deploy/fly/fly.toml fly.toml`), set the app name and `SMTP_DOMAIN`, and run the commands
below in `bridge/`.

1. **Create the app.**

   ```sh
   fly apps create my-mail-bridge
   ```

2. **Set the secrets.** The same secret goes into the Convex deployment (see above).

   ```sh
   fly secrets set CONVEX_SITE_URL=https://<deployment>.convex.site \
     MAIL_BRIDGE_SECRET=<secret>
   ```

3. **Deploy**, then run two machines.

   ```sh
   fly deploy
   fly scale count 2
   ```

   The configuration has to expose ports 993 and 465 with the `tls` and `proxy_proto` handlers,
   pointing at the internal ports 1143 and 1465. Two machines have each their own mirror; that
   is correct, because each one asks the edge.
4. **Addresses.** `fly ips list` shows what the app has. If it has none, allocate them:

   ```sh
   fly ips allocate-v6
   fly ips allocate-v4 --shared
   ```

   According to Fly's documentation, a shared IPv4 address is enough for ports 993 and 465 too,
   because the TLS handler distributes by server name (SNI). Allocate a dedicated one
   (`fly ips allocate-v4`, which costs extra) only if a mail client without SNI turns up.
5. **Certificate.**

   ```sh
   fly certs add mail.example.com
   ```

   The command names the DNS records it needs.
6. **DNS.** Create `mail.example.com` as an A and an AAAA record pointing at the addresses from
   step 4. Do not put an HTTP-only CDN proxy in front of it: those do not forward ports 993 and
   465. Check that the certificate has been issued:

   ```sh
   fly certs show mail.example.com
   ```

7. **Reachability.** See "Checking reachability" below.
8. **Accept the PROXY header only from the proxy.** This is a must before the first mailbox of
   someone else is served. See "Restricting the PROXY header" below. In `fly.toml` also set
   `SMTP_DOMAIN` to the public host name (`mail.example.com`).
9. **First test: does the proxy hold a silent IDLE connection for 29 minutes?** See "The 29-minute
   IDLE test" below.
10. **Observe.** See "What to watch" below.

Notes for `fly.toml`:

- `ADMIN_ADDR` has to listen on `[::]:8080` for `fly proxy` to reach it. That port is not
  reachable from the internet, but it is reachable from the private network of your Fly
  organization: any other app there can read the counters.
- If your `fly.toml` binds the mail ports (`IMAP_ADDR`, `SMTP_ADDR`) to IPv4 only and the Fly
  proxy cannot reach them that way, set them to `[::]:1143` and `[::]:1465`, and set
  `PROXY_TRUSTED_CIDRS` right away.
- `GOMEMLIMIT=400MiB` keeps the Go heap below the memory of a 512 MB machine (see "Memory").

## Docker Compose behind HAProxy on your own server

If you want TLS to end on a machine you control, run the bridge and a proxy with Docker Compose.
The files are in `bridge/deploy/compose/`: `docker-compose.yml` and `haproxy.cfg`. Their contents
are an example. What the proxy has to do, whatever proxy you use:

- Terminate TLS on ports 993 and 465 with a certificate for your mail host name. Renewing the
  certificate is your job.
- Forward the decrypted TCP stream to the bridge (`1143` for IMAP, `1465` for SMTP) with
  `send-proxy-v2` (HAProxy's name for a PROXY protocol v2 header).
- Be the only thing that can reach the bridge's mail ports. Do not publish 1143 and 1465 on the
  host.
- Set `PROXY_TRUSTED_CIDRS` of the bridge to the network the proxy connects from (the subnet of
  the Compose network). A PROXY header from anywhere else is refused.
- Keep the admin port (`ADMIN_ADDR`, `/healthz` and `/metrics`) private: do not publish it, or
  publish it on `127.0.0.1` only. It has no authentication.

Then set `CONVEX_SITE_URL` and `MAIL_BRIDGE_SECRET` for the bridge container and follow the
checks below, from "Checking reachability" on.

## Checking reachability

With TLS, both ports must greet:

```sh
openssl s_client -quiet -connect mail.example.com:993
openssl s_client -quiet -connect mail.example.com:465
```

The first must show `* OK [CAPABILITY IMAP4rev1 ...] IMAP server ready`, the second a
`220 ... ESMTP`. If no greeting comes, look in the logs for `conn_rejected` with the code
`proxy_missing`: the proxy does not send a PROXY header (on Fly, the `proxy_proto` handler is
missing).

Counter-check without TLS: these must show no greeting at all.

```sh
nc -w 5 mail.example.com 993 </dev/null
nc -w 5 mail.example.com 465 </dev/null
```

## Restricting the PROXY header

Without `PROXY_TRUSTED_CIDRS`, the bridge believes the PROXY header from anyone who reaches the
mail ports directly. That peer could fake a client address, which lets it get around the login
throttle or turn it against someone else, and it could talk to the bridge without TLS. The bridge
logs this at every start as a `warning` with the code `proxy_peers_untrusted`.

1. Find out which network the connections come from. With the admin port reachable (on Fly:
   `fly proxy 8080:8080`):

   ```sh
   curl localhost:8080/metrics
   ```

   Read `proxyPeers`: it lists the networks PROXY headers arrived from.
2. Set that network as `PROXY_TRUSTED_CIDRS`, a comma-separated list of CIDR ranges
   (for example `192.0.2.0/24`, or `2001:db8::/32`), and redeploy.
3. Check the log: the `proxy_peers_untrusted` warning must be gone. From now on a connection from
   outside the list is closed and logged as `conn_rejected` with the code `proxy_peer_untrusted`
   (and counted in `rejectedProxy`).

## The 29-minute IDLE test

A mail client can leave an IMAP connection in IDLE for a long time without any traffic. Test
whether your proxy holds such a connection, with a test mailbox:

1. Set `IDLE_KEEPALIVE_SECONDS=0`. Otherwise the bridge's keepalive line keeps the connection
   awake by itself and the test proves nothing.
2. Then:

   ```text
   openssl s_client -quiet -connect mail.example.com:993
   a LOGIN user@example.com <device password>
   b SELECT INBOX
   c IDLE
   ... do nothing for 29 minutes ...
   DONE
   ```

3. If `c OK IDLE completed` comes after `DONE`, the proxy holds. If the connection drops earlier,
   set `IDLE_KEEPALIVE_SECONDS` below the time you observed (the default is 240) and repeat the
   test: now a line `* FLAGS (...)` must arrive every few minutes.
4. Put the value back to what you want to run with.

## What to watch

`GET /healthz` returns `ok` (and `503` while the bridge shuts down). `GET /metrics` is JSON.
Logs are one JSON line per event.

Tripwires in `/metrics`:

| Counter | What it means |
| --- | --- |
| `mirror.fullLoads` and `mailboxes.<hash>.fullLoads` | full folder fetches; they should be rare |
| `mirror.uidValidityChanges`, `mailboxes.<hash>.uidValidityChanges` | folders were renumbered |
| `imap.appendsRejected` | `APPEND`s the bridge refused |
| `panics` | should stay 0 |
| `edge.<path>.errors` | calls to the edge that failed or were not 2xx |
| `edgeFaults` (`http400`, `http413`, `http500`, `oversize`, `notSent`) | all five should stay 0 |
| `mirror.noFolder` | the edge did not know `INBOX` or `Sent` |
| `proxyPeers` | networks PROXY headers come from; compare with `PROXY_TRUSTED_CIDRS` |

Useful log events: `edge_fault` (path and status), `conn_rejected` (with a reason code),
`warning`. The log never contains addresses, subjects, contents, passwords, session tokens or
client addresses. To find the lines of a mailbox, compute its hash; see "Observability" in
`../bridge/README.md`.

## Memory

The bridge keeps in memory: the raw message cache (`RAW_CACHE_BYTES`, default 64 MiB), messages
in flight (`WORK_MEM_BYTES`, default 128 MiB), the mirror (`MIRROR_MAX_ENTRIES`, default
400,000 rows, around 30 MiB) and the Go runtime. The smallest machine with 512 MB is enough for
that. If you raise the limits, raise the machine with them. Move to 1 GB as soon as more than a
handful of mailboxes read large messages at the same time.

## Who can read what

TLS ends at the proxy, and behind it the traffic is plaintext up to the bridge. The operator of
the proxy can therefore technically read everything a mail client sends and receives: device
passwords and the content of messages. With Fly.io that is Fly; with the Compose setup it is
whoever runs that server.

- If the proxy is operated by a third party, list that party as a processor and sign a data
  processing agreement with it before you serve mailboxes of other people.
- To remove the third party from the path, run the proxy yourself, as in the Compose setup. The
  bridge itself has no TLS and no setting for certificates, so terminating TLS inside the bridge
  is not an option today.
- A device password opens exactly one mailbox, and it can be revoked individually. A revoked
  password ends its connections: the edge answers `401 session`.

## Acceptance checklist

Before the first real mailbox, with a test mailbox, on a computer and on a phone. Set the mail
client's Drafts and Trash mailboxes by hand to be kept on the device (in Apple Mail, under
Mailbox Behaviors). A configuration profile cannot do this, because Apple's profile schema has
no key for it.

- [ ] Set-up (profile or by hand): both folders show up, `Sent` is used as the sent folder
- [ ] Reply: the message goes out and shows up in the app on the right conversation
- [ ] Sent copy: exactly one message in `Sent`, no duplicate, no error when it is filed
- [ ] Retry after a dropped connection (airplane mode while sending): no second send
- [ ] Read and unread in both directions (mail client and app)
- [ ] Delete in the mail client: the message disappears there and stays in the app
- [ ] Revoking the device password with a warm cache: the connection ends, nothing is readable
- [ ] Large message: send an attachment near 5 MiB, read a large incoming message
- [ ] Restart of all machines: the mail client reconnects and loads nothing twice
- [ ] IDLE: a new message appears on the computer without fetching; the phone fetches on its
      schedule

## Rolling out when the contract gains fields

When the edge contract gains a field, deploy in this order: first the Convex side, then the
bridge. An edge that does not know a field rejects it with `400 request`, so a bridge that
sends it too early fails on every call. Revision 1.2 is an example: an edge that predates the
`bridge` field of `/login` rejects it.

## Switching off

- The edge closes at once when `MAIL_BRIDGE_SECRET` is removed from the Convex deployment:
  it then answers everything with `401`.

  ```sh
  npx convex env remove MAIL_BRIDGE_SECRET --prod
  ```

- The bridge can be stopped. On Fly:

  ```sh
  fly scale count 0 -a my-mail-bridge
  ```

Neither affects the app itself.
