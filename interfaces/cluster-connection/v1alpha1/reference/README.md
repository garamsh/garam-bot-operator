# cluster-connection v1alpha1 reference server

The in-memory server of `../README.md`: what it serves, how it is run, and where it stands in for garam.

## What it serves

- The four routes of `../openapi.yaml`, over TLS from a root it generates for one run.
- The `machine-token.v2` exchange at the `tokenUrl` it answers, `<base URL>/oauth/token`, verifying the `private_key_jwt` assertion itself.
- The server fixture of `../conformance.md` §The server fixture, on a Unix socket only the fixture subcommands reach.

All state is in memory and ends with the process.

## Running it

- `cluster-connection-reference --dir <dir> serve` writes `ca.pem` (the root to trust), `url` (the base URL) and `fixture.sock` into `<dir>`, then prints `{"baseUrl", "trustFile"}` on one line once it accepts.
- `cluster-connection-reference --dir <dir> <subcommand> ...` runs one fixture subcommand against the server serving `<dir>`.
- `serve --help` lists its flags: the listen address, the host name in the base URL, the declarations page size (set it low to exercise DECL-5's `more`), and the registration bound per source address per minute (REG-8).

## Where it differs from garam

- **Records:** every record is in the organization `reference`, and its `clientId` is its GRN.
- **DECL-8 and DECL-9 are judged by member name** until interface 1 names its fields (`garamsh/garam-bot#1193`). In `spec`, at any depth, a member named `images` or ending in `image` names an image, and a member ending in `secret` or `secretRef` must be a `<secret-name>/<key>` string. Case is ignored.
- **AUTH-4 holds by construction:** it serves no other operation. **AUTH-5 is not served:** the bot-token mint is garam's, and its shape is not part of this interface.
- **Tombstones are kept until `expire-cursors`,** which satisfies DECL-6's 30 days.
