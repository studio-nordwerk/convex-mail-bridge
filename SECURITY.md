# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub instead: on the repository page open **Security** and choose
**Report a vulnerability**. Include:

- what you found and where (component, client layer, bridge, documentation),
- how to reproduce it, ideally against the example app or the development bridge (`bridge/dev.sh`),
- what an attacker could gain.

You will get an answer within five working days. Please give us reasonable time to fix the
problem before you publish details. We will credit you in the release notes if you wish.

## Scope

In scope: this repository, that is the Convex component, the client layer and the Go bridge.

Out of scope: the app you connect (its access callbacks, its sending, its storage), your hosting
and your TLS proxy. [docs/security.md](docs/security.md) lists what the package relies on your
app to get right.

## Supported versions

Before 1.0 only the latest release receives fixes.
