# mail-bridge

The bridge between mail clients and a Convex app. Towards the mail client it speaks IMAP and SMTP
submission; everything it serves it fetches from the app over an HTTP contract, the edge. It is
the Go service of the repository `github.com/studio-nordwerk/convex-mail-bridge`
(Go module `github.com/studio-nordwerk/convex-mail-bridge/bridge`, binary `mail-bridge`). The
Convex side is the package `@nordwerk/convex-mail-bridge` in `src/`.

- The contract with the edge: [`../docs/edge-contract.md`](../docs/edge-contract.md)
- Deploying and running it: [`../docs/operations.md`](../docs/operations.md)
- What it does not do, and what has not been verified: [`../docs/limits.md`](../docs/limits.md)

## What it is and what it is not

- **It is** a Go service without a disk and without a database. It serves two fixed folders,
  `INBOX` and `Sent`. Mail clients can read, set flags, delete (`\Deleted` plus `EXPUNGE` hides a
  message from the mail client), use IDLE, and send through SMTP submission.
- **It is not** a store. Messages, UIDs, flags and device passwords live in Convex. The bridge
  keeps in memory only a mirror of the folder rows (UID, flags, size, date) and the raw messages
  it fetched last, both with fixed upper bounds. A restart loses nothing.
- **It has no** TLS (TLS ends at the proxy in front of it), no STARTTLS, no relay and no folder
  management.

Access rule: a connection reads the mirror and the cache only for the mailbox it logged in to
itself, and only while the edge confirmed its session within the last few seconds
(`SESSION_FRESH_SECONDS`, default 10). When the edge reports a session as invalid, exactly that
connection ends. The bridge does not keep the password after login.

At login the edge tells the bridge which mailbox it is (`mailbox`, an opaque key). The mirror,
the raw cache and all counters hang on that key and on the `uidValidity` of the folder, not on
the address that was typed. Two addresses of the same mailbox share one mirror. An address that
later names a different mailbox sees nothing of the old one. A login whose answer has no
`mailbox` counts as failed. The bridge fetches raw messages with the `uidValidity` its
connection works on; if the edge answers `409` (the folder was renumbered), the bridge drops the
mirror of that folder together with the cached messages and closes the connections that have it
open. `UIDNEXT` comes from the edge at every sync.

## What the bridge sends to the edge

The edge rejects any body that carries a field it does not know (`400 request`) and any JSON body
over 64 KiB (`413`). So the bridge builds every call from a fixed set of fields
(`internal/edge`), exactly those in the contract:

| Path | Body | Largest body |
| --- | --- | --- |
| `/login` | `address`, `password`, `clientIp` | under 1 KiB |
| `/folders` | `{}` | 2 bytes |
| `/entries` | `folder`, `limit`, `afterUid` (from the second page on) | - |
| `/changes` | `folder`, `sinceModseq`, `limit` | - |
| `/raw` | `folder`, `uid`, `uidValidity` | - |
| `/store` | `folder`, `uids`, `add`, `remove` | 18.3 KiB (500 UIDs, 64 flags of 100 characters) |
| `/expunge` | `folder`, `uids` | 5.4 KiB (500 UIDs) |
| `/append` | `folder` (`Sent`), `messageId`, `digest` | under 2 KiB |
| `/seq` | `sessions` | 50.6 KiB (200 tokens of 256 characters; the edge issues 43) |

`/submit` carries the raw message as its body and the envelope in headers; see the contract.
This bridge implements version 1 of the contract: it does not send the `bridge` field of
`/login`, so the edge treats it as version 1.

- `clientIp` is always a bare IPv4 or IPv6 address, without port, brackets or zone. An IPv4
  address wrapped in IPv6 goes out as IPv4. A PROXY header with a zone is rejected.
- Larger jobs are split: at most 500 UIDs and 64 flags per list and call, 200 tokens per `/seq`.
  A session token that would be longer than 256 characters, or does not fit a header, counts as
  a failed login. A JSON body over 64 KiB is not sent but counted (`edgeFaults.oversize`); the
  tests compute the largest bodies.
- What the edge would not accept as a request at all, the bridge answers itself in the way that
  fits the mail client, instead of producing a `400`: an address over 320 or a password over 256
  characters, or an empty one, is a failed login; a flag over 100 characters is not a valid
  flag; a message without Message-ID, or with one over 500 characters, is unknown for `APPEND`
  (`NO [CANNOT]`). These limits are not in the contract itself but in the edge's checks
  (`BRIDGE_LIMITS` in `src/client/edge.ts`); in the bridge they are in one place,
  `internal/edge/limits.go`.

## What the edge answers, and what the mail client hears

| Answer of the edge | IMAP | SMTP |
| --- | --- | --- |
| `401 auth` at `/login` | `NO [AUTHENTICATIONFAILED]` | `535` |
| `429 throttled` (wait from `retryAfterSeconds` or `Retry-After`) | `NO [UNAVAILABLE]` | `454` |
| `503 preparing` | `NO [UNAVAILABLE]`, "being prepared" | `454` |
| `401 session` | this one connection ends (`BYE`) | `535`, connection ends |
| `404 gone` at `/raw` | the message is missing from the answer | |
| `404 folder` | `NO [UNAVAILABLE]`; mirror of the folder dropped, connections in it end | |
| `409 uidvalidity` at `/raw` | mirror of the folder dropped, connections in it end | |
| `410 resync` at `/changes` | the folder is read again, cached messages of the folder dropped | |
| `409 unknown`, `403 folder` at `/append` | `NO [CANNOT]` | |
| `/submit` rejection with `smtpCode` (not `500`) | | that code |
| **`400`, `413`, `500`** and everything else | `NO [UNAVAILABLE]` | `451` (login: `454`) |

`400`, `413` and `500` are not a fault of the mail client but of the exchange between bridge and
edge. The bridge retries nothing by itself, counts each of them (`edgeFaults.http400`,
`http413`, `http500` in `/metrics`) and writes an `edge_fault` line with path and status to the
log. A `/submit` rejection that names its SMTP code (for example `413` with `552`, message too
large) is the edge talking to the mail client and does not count there; it still shows in
`edge./submit.status`.

The throttle: the edge throttles only invalid login attempts, never a valid device password. So
after a `429` the bridge remembers exactly that one attempt (address, client and password
together, as an HMAC with a key that never leaves the process) and answers its repetition itself
until the wait is over. A different password for the same address from the same connection
source always goes to the edge. A second device with the right password is not locked out because
the first one tries an old one.

A state "from the future" (`410`, because `sinceModseq` is above the folder's state) means the
folder was reset at the edge. The bridge reads it again. If a known UID then carries a different
size or arrival date, it is a different message under an old number: the connections that have
the folder open end and nothing cached is kept (`mirror.inconsistent`).

## Layout

| Directory | Contents |
| --- | --- |
| `cmd/mail-bridge` | the service |
| `cmd/devstack` | bridge and stub edge in one process, for local development only |
| `cmd/edgeprobe` | probe program for `test/against-edge.sh`: checks a running bridge over IMAP |
| `internal/guard` | in front of the listeners: PROXY protocol, connection limits, limits before and after login |
| `internal/imapd`, `internal/smtpd` | the sessions on top of `go-imap` v2 and `go-smtp` |
| `internal/mirror`, `internal/rawcache`, `internal/budget` | mirror per mailbox, raw message cache, memory limit for messages in flight |
| `internal/edge` | the client of the edge |
| `internal/edgestub` | stub edge: reproduces the contract in memory |
| `test/` | `mbsync.sh` (real client), `fuzz-mutation-check.sh` (random test against a built-in fault), `against-edge.sh` (probe against a given edge) |

## Environment

The bridge is configured through the environment only: no file, no flags. It does not start if
`CONVEX_SITE_URL` or `MAIL_BRIDGE_SECRET` is missing or invalid. A bad value for any other
variable is also an error at start (the message names the variable, never the value). Defaults
and ranges below are those of `internal/config/config.go`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CONVEX_SITE_URL` | none | `https://<deployment>.convex.site`. Plain `http` only for loopback hosts. No credentials, query or fragment |
| `MAIL_BRIDGE_SECRET` | none | secret shared with Convex, at least 32 characters, no blanks (below that the edge is off and answers only `401`) |
| `IMAP_ADDR`, `SMTP_ADDR` | `:1143`, `:1465` | internal listen addresses (plaintext behind the proxy) |
| `ADMIN_ADDR` | `127.0.0.1:8080` | `/healthz` and `/metrics`; never public |
| `PROXY_PROTOCOL` | `required` | `required`: connections without a valid PROXY header (v1 or v2) are dropped. `off` only for local use |
| `PROXY_TRUSTED_CIDRS` | empty | comma-separated CIDR ranges. When set, a PROXY header is accepted only from these networks. Empty with `required`: the bridge believes the header from anyone who reaches the mail ports, and logs a `warning` with `proxy_peers_untrusted` at every start |
| `IMAP_MAX_CONNS`, `IMAP_MAX_CONNS_PER_IP` | 1000, 30 | connections in total and per client (IPv6: per /64); 1 to 100000 |
| `SMTP_MAX_CONNS`, `SMTP_MAX_CONNS_PER_IP` | 200, 10 | the same for SMTP |
| `PREAUTH_TIMEOUT_SECONDS` | 30 | time allowed until login, then the connection is dropped; 1 to 600 |
| `PREAUTH_MAX_LINE_BYTES` | 2048 | longest line before login; 256 to 8192 |
| `PREAUTH_MAX_BYTES` | 16384 | most bytes before login; 1024 to 1048576 |
| `PREAUTH_MAX_LITERAL_BYTES` | 128 | largest literal before login; 0 to 1024 |
| `MAX_AUTH_FAILURES` | 3 | failed logins per connection; 1 to 20 |
| `IMAP_MAX_LINE_BYTES` | 32768 | command length after login; 1024 to 49152 |
| `APPEND_MAX_BYTES` | 6 MiB | largest `APPEND`, announced as `APPENDLIMIT`; 1 KiB to 64 MiB |
| `SEARCH_MAX_DEPTH` | 16 | deepest nesting of `SEARCH` criteria; 1 to 64 |
| `IMAP_MAX_SET_RANGES` | 4096 | most ranges in one sequence set; 16 to 65536 |
| `SMTP_READ_TIMEOUT_SECONDS` | 300 | idle time of an SMTP connection and longest single transfer; 10 to 3600 |
| `SMTP_DOMAIN` | `mail-bridge` | the name the SMTP server announces; set the public host name |
| `SMTP_MAX_MESSAGE_BYTES` | 5 MiB | `SIZE` in EHLO; larger gets `552`; 1 KiB to 64 MiB |
| `SMTP_MAX_RECIPIENTS` | 50 | recipients per message; 1 to 1000 |
| `RAW_CACHE_BYTES` | 64 MiB | raw messages in memory (LRU); 1 MiB to 4 GiB |
| `RAW_MAX_BYTES` | 32 MiB | largest raw message the bridge will fetch; 1 MiB to 256 MiB |
| `RAW_FETCH_CONCURRENCY` | 6 | parallel raw fetches; 1 to 64 |
| `WORK_MEM_BYTES` | 128 MiB | messages in flight outside the cache (FETCH, APPEND, DATA); one mailbox at most a quarter. Must be at least four times the largest of `RAW_MAX_BYTES`, `APPEND_MAX_BYTES` and `SMTP_MAX_MESSAGE_BYTES`; 32 MiB to 16 GiB |
| `MIRROR_MAX_ENTRIES` | 400000 | folder rows of all mirrors together (LRU over mailboxes without a connection); 1000 to 50000000 |
| `EDGE_TIMEOUT_MS` | 15000 | time limit of a call to the edge; 50 to 120000 |
| `EDGE_LONG_TIMEOUT_MS` | 60000 | the same for `raw` and `submit`; 50 to 300000 |
| `SEQ_INTERVAL_MS` | 2000 | how often the change counters are polled, only while connections are open; 100 to 60000 |
| `SESSION_FRESH_SECONDS` | 10 | how long a connection may rely on the edge's last confirmation; raised to twice `SEQ_INTERVAL_MS` plus 1 s if lower; 1 to 600 |
| `IDLE_KEEPALIVE_SECONDS` | 240 | keepalive line during IDLE; `0` turns it off; 0 to 1700 |
| `TCP_KEEPALIVE_SECONDS` | 60 | TCP keepalive towards the proxy; `0` turns it off; 0 to 3600 |
| `SHUTDOWN_GRACE_SECONDS` | 10 | how long a shutdown waits for connections to end; 1 to 120 |

The Dockerfile exposes 1143, 1465 and 8080. Build the production image with
`docker build -t mail-bridge .` in this directory; the last stage (`runtime`) is the one for
operation: a static binary on a distroless base, running as a non-root user, without a shell.

## Running locally

All you need is Docker. One command builds the development image and starts the bridge together
with a stub edge in the container `mail-bridge-dev`:

```sh
./dev.sh            # ./dev.sh stop ends it
```

Afterwards: IMAP on `127.0.0.1:3143`, SMTP on `127.0.0.1:3465`, metrics on
`http://127.0.0.1:3190/metrics`. There is no TLS and no PROXY protocol. There are two invented
mailboxes; their addresses and device passwords are written to `.dev-credentials` (mode 600, in
`.gitignore`, never commit it). The first mailbox is the large one: 300 messages in `INBOX`
(text and HTML, about two dozen with an attachment, four of them 0.8 to 2 MB, umlauts and emoji in
subjects and names, four longer threads with `In-Reply-To` and `References`, 30 unread) and 40 in
`Sent`. The second has 30 and 6. A message sent from one mailbox to the other arrives there.
Everything is in memory: after a restart the initial state is back.

To deliver a new message while a mail client is connected (to watch IDLE):

```sh
./dev.sh mail <address> "<subject>"            # optionally followed by the body text
DEV_TICK_SECONDS=60 ./dev.sh                   # or at start: one new message every minute
```

**Trying Apple Mail against it** (no TLS, local only). This has not been walked through with
Apple Mail; the setup was cross-checked with `imaplib`, `smtplib` and mbsync only. The menu
names below follow the German macOS user interface and may differ. Mail > Settings > Accounts >
"+" > Other Mail Account, with the address and device password from `.dev-credentials`.
Automatic detection fails (the domain does not exist). Continue and create the account anyway,
then enter the servers by hand:

| Field | Value |
| --- | --- |
| Account type | IMAP |
| User name (incoming and outgoing) | the full address |
| Incoming mail server | `127.0.0.1` |
| Outgoing mail server | `127.0.0.1` |

Mail says it cannot connect securely. Under Accounts > Server Settings, for both servers, turn
off "Automatically manage connection settings" and enter port **3143** (incoming) and **3465**
(outgoing), **TLS/SSL off**, authentication **Password**. Under "Mailbox Behaviors" set Drafts
and Trash to "On My Mac" and choose the folder `Sent` for sent messages.

## Tests

Go is not needed on the host; everything runs in Docker. From this directory:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.24 sh -c 'go vet ./... && go test -race ./...'
./test/mbsync.sh               # isync (mbsync) against the bridge and the stub edge
./test/fuzz-mutation-check.sh      # the random test must find a fault that was built in on purpose
```

- The tests start the bridge and the stub edge in the same process and drive them with the
  `go-imap` client (`imapclient`), an SMTP client and, where single lines matter, raw
  connections.
- `test/mbsync.sh` runs `mbsync`, a strict real IMAP client, in a container: first sync, second
  run without fetches, changes on both sides, a stable follow-up run. Output goes to
  `test/out/mbsync.log`.
- `test/fuzz-mutation-check.sh` is the counter-check of the random test. It builds the bridge with a
  deliberate fault (build tag `faultinject`: removals are reported to the connections in the
  wrong order) and expects `TestRandomConcurrentChanges` to find it. If the test does not fail,
  the test is worthless and the script exits with an error.
- The stub edge (`internal/edgestub`) checks requests as strictly as the real edge: an unknown
  field, a wrong type or a value out of range gives `400 request`; a body over 64 KiB gives `413`;
  `clientIp` only as a bare address; a session token only in its shape (43 characters); an
  unknown folder gives `404`; a state from the future gives `410`; `429` carries `Retry-After`;
  a valid password is never throttled. Every bridge test fails if the stub edge rejected even
  one request, or if the bridge could not send a call.

### Probing a real edge

`test/against-edge.sh` starts the bridge in Docker (container `mail-bridge-edge`, ports 3243,
3565 and 3290 on `127.0.0.1`, next to `mail-bridge-dev`) against a given edge, for example a
local Convex backend running your app, and checks it over IMAP:

```sh
EDGE_URL=http://host.docker.internal:14211 MAIL_BRIDGE_SECRET=... \
MAILBOX_ADDRESS=... DEVICE_PASSWORD=... EXPECT_INBOX=12 EXPECT_SENT=3 \
./test/against-edge.sh
```

| Input | Meaning |
| --- | --- |
| `EDGE_URL` | address of the edge as seen from the container (without `/bridge/v1`) |
| `MAIL_BRIDGE_SECRET`, `MAILBOX_ADDRESS`, `DEVICE_PASSWORD` | the secret, a mailbox, one device password |
| `SECOND_ADDRESS`, `SECOND_PASSWORD` | a second mailbox: checks the separation |
| `EXPECT_INBOX`, `EXPECT_SENT` | expected number of messages at the start |
| `SKIP_IDLE=1` | skip the IDLE step |
| `ALLOW_EXPUNGE=1` | mark a message deleted and remove it with `UID EXPUNGE` (the message from the IDLE step, else the newest; `EXPUNGE_UID=<n>` picks one) |
| `MBSYNC=1` | then run mbsync against the same bridge (pull only): first sync byte for byte, second run without fetches |
| `KEEP=0` | remove the container at the end (otherwise it keeps running; `./test/against-edge.sh stop`) |
| `MAX_MAILS`, `IDLE_WAIT_SECONDS`, `IDLE_GRACE_SECONDS` | read 200 messages per folder completely; wait 900 s for the signal; 30 s grace after it |
| `IMAP_PORT`, `SMTP_PORT`, `ADMIN_PORT`, `DOCKER_RUN_ARGS` | other ports; extra arguments for `docker run` |

What is checked, assuming nothing about the edge beyond the contract: login with an invented and
with the right password; `LIST`; `STATUS` and `SELECT` of both folders against each other and
against the expectation; `UID FETCH 1:* (UID FLAGS RFC822.SIZE INTERNALDATE)`; per message
`BODY.PEEK[]` with a length equal to `RFC822.SIZE`, a readable `ENVELOPE` and `BODYSTRUCTURE`,
and that reading does not change a flag; `\Seen` on an unread message and back, `\Flagged` on
and off, each reported to and visible in a second connection, `UNSEEN` from the edge's counter;
IDLE; `UID EXPUNGE`; reconnecting without a full fetch and without fetching a read message again
(from `/metrics`); with a second mailbox: its own password, its own identifier, the same UID is
fetched from the edge on its own, a change in one does not reach the other; finally the bridge's
counters per path and status, with times. A `400`, `413` or `500` from the edge during the run
is an error. The output has folders, UIDs, sizes and counters, never addresses, passwords,
subjects or contents. Exit status 0 if everything passed, 1 on errors, 2 on a wrong call.

About the IDLE step: the script prints "IDLE steht" (IDLE is waiting). Now deliver a message
into the mailbox from outside, then give the signal: `touch /tmp/against-edge.trigger`, or press
Enter at the terminal. The probe measures when `EXISTS` arrived, relative to the start of IDLE
and to the signal. A signal before "IDLE steht" is discarded.

What the run changes at the edge: one failed login attempt (two with a second mailbox); the
flags `\Seen` and `\Flagged` on one message in `INBOX`, set and taken back (the edge hands
`\Seen` to the app's `onSeen` callback, so this also moves the app's own read state there and
back); and, with `ALLOW_EXPUNGE=1`, one message hidden from mail clients.

The image for this (`docker build --target probe`) contains the bridge built with
`-tags localedge`. Only built this way does the bridge speak plain `http` to the edge on a host
other than `localhost` (from inside the container the Mac is `host.docker.internal`). The
production image is built without that tag and has no setting for it. The secret is an
environment variable in the container (`docker inspect` shows it); use the secret of a local
backend.

Without a Convex backend you can try the script against the stub edge: a second container from
the development image with `DEV_EDGE_ADDR=0.0.0.0:3211` and `DEV_EDGE_SECRET=<32 or more
characters>` exposes its stub edge (`-p 127.0.0.1:<port>:3211`). `./dev.sh mail ...` corresponds
there to `docker exec <container> /mail-bridge-dev mail <address> "<subject>"`.

## Observability

- `GET /healthz` on the admin port: `ok`; `503` while shutting down.
- `GET /metrics` (JSON): connections, logins and failed attempts per protocol; calls to the edge
  per path with count, duration, bytes and the answers by status code (`edge.<path>.status`;
  `none` = no answer); `edgeFaults` with one counter each for `400`, `413` and `500` from the
  edge and for calls the bridge could not send (`oversize`, `notSent`): in operation all five
  are 0, anything else is a fault between bridge and edge; `mirror.noFolder` (the edge did not
  know `INBOX` or `Sent`); the mirror, the cache and working memory (`workMem`); per mailbox
  (shortened hash of the mailbox key) calls, bytes, full folder fetches and `uidValidity` changes;
  rejected `APPEND`s; panics; `proxyPeers` (the networks PROXY headers come from).
- Logs: one JSON line per event, with time, event, code, hash and counters. No addresses,
  subjects, contents, passwords, session tokens or client addresses, and not even the mailbox
  key. The log API has no free-text field, so these cannot get in by accident. Example:

  ```json
  {"t":"2025-01-15T09:30:00.000Z","ev":"edge_fault","code":"http","status":400,"path":"/store"}
  ```

  `mb` is the shortened hash of the mailbox key (the entries under `mailboxes` in `/metrics`
  are named the same way):

  ```sh
  printf '%s' <mailbox key> | shasum -a 256 | cut -c1-12
  ```

  `addr` is the hash of the address:

  ```sh
  printf '%s' <address> | tr A-Z a-z | shasum -a 256 | cut -c1-12
  ```

  `addr` appears for failed logins, and once per successful login next to `mb`, so you can get
  from an address to its mailbox.

## Shutdown

`SIGTERM` (and `SIGINT`): no new connections; open connections get `* BYE` (IMAP) or `421`
(SMTP); then the process ends, after at most `SHUTDOWN_GRACE_SECONDS`. A submission that is in
flight to the edge at that moment is cut off. The mail client tries again; that the retry does
not send the message twice is the job of the app's `submit` callback (see the contract).
