# Contributing

Issues and pull requests are welcome. For anything larger than a fix, please open an issue first
so we can agree on the direction.

## Setup

```sh
npm install
npm run check
```

`npm run check` builds the package, typechecks it and the example app, lints, and runs the tests.
The tests use `convex-test`; no Convex deployment is needed. The example app imports the package
by its name, which resolves to `dist/`, so build before you run the example's tests on their own.

The files under `_generated/` are checked in. After changing the functions of the component or
the modules of the example app, regenerate them with a Convex development deployment of your own
(`npx convex dev` in this folder runs the example app; `npm run build:codegen` regenerates the
component's files).

The bridge needs only Docker:

```sh
cd bridge
docker run --rm -v "$PWD":/src -w /src golang:1.24 sh -c 'go vet ./... && go test -race ./...'
./dev.sh    # bridge and an in-memory edge on localhost, see bridge/README.md
```

## Ground rules

- The HTTP contract between bridge and app is [docs/edge-contract.md](docs/edge-contract.md).
  A change to it changes that document first, then both sides, and the limits in
  `src/client/edge.ts` and `bridge/internal/edge/limits.go` together.
- Nothing may log or store an address, a subject, message content, a password, a token or a
  client IP.
- A new check needs a test that fails without it.
- English for everything a user can see, in code, comments, test names and script output alike.
  The umlauts, emoji and long words in names, subjects and labels in the tests and in the
  development stack's invented mails are deliberate: they exercise header encoding.
