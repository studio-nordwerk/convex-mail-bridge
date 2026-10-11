# Maintaining

For maintainers. Contributors do not need any of this.

## Where the code comes from

The component (`src/component`) and the bridge (`bridge/`) started inside a private application
and still receive changes from there; the maintainers carry them over. The client layer
(`src/client`), the example app, the deployment examples and all documentation are written here.

After changes were carried over:

1. `npm run check` and the Go tests.
2. If the component's functions changed, regenerate `src/component/_generated`
   (`npm run build:codegen` against a development deployment or a local backend).
3. Changes to the edge functions, the routes and the request checks belong in `src/client`, with
   a test in `example/convex`.
4. Search what came in for internal names and for non-English text in anything a user sees.
5. Update `docs/edge-contract.md` and `CHANGELOG.md`.

## Before the first publication

- [x] Name: Convex Mail Bridge, `@nordwerk/convex-mail-bridge`.
- [x] Generated files: `example/convex/_generated` and `src/component/_generated` were regenerated
      against a local backend and are identical to what is checked in.
- [x] The example app ran on a local, self-hosted Convex backend with the bridge in front of it;
      `bridge/test/against-edge.sh` and `mbsync` passed (see `docs/limits.md`).
- [ ] Try one real mail client against the example app, including sending.
- [x] 0.1.0 is on npm, published by hand (see below); the tag `v0.1.0` created the GitHub release.
- [ ] Add this repository and `release.yml` as trusted publisher in the package's settings on
      npmjs.com, so the next version can be released by tag.
- [x] The preflight check of the Convex components directory passed against the public repository
      (8 of 8 critical requirements, 6 of 6 suggested improvements).

`SECURITY.md` relies on private vulnerability reporting being enabled in the repository settings.

## Releasing

The first version is published by hand, because npm only accepts a trusted publisher for a package
that exists:

```sh
npm ci && npm run build:clean && npm run typecheck && npm run lint && npm test
npm pack                      # inspect the tarball
npm publish --access public
```

Then, in the package's settings on npmjs.com, add this repository and the workflow file
`release.yml` as trusted publisher. From there on a release is a tag:

1. Set the version in `package.json`, give the entry in `CHANGELOG.md` the heading
   `## <version>`, commit.
2. `git tag -a v<version> -m "<version>" && git push --follow-tags`

`.github/workflows/release.yml` runs the checks, publishes to npm unless that version is there
already, and creates the GitHub release from the changelog entry. For the hand-published first
version the tag only creates the release.

The bridge is released as a container image built from `bridge/Dockerfile`; tag it with the
same version.
