# cluster-connection v1alpha1

The protocol between garam's connection service (the server) and a connector running in a user's cluster (the client): registration, key rotation, declarations sent to the cluster, and reports sent back. Decided in ADR 0087 to ADR 0092 of `garamsh/garam-bot-operator`. The schemas are `openapi.yaml` beside this file. The conformance tests are `conformance.md`.

The key words MUST, MUST NOT, SHOULD and MAY are used as RFC 2119 defines them. Each requirement carries an identifier that `conformance.md` maps to a test.

## Status and tier

- **Version:** `v1alpha1`, pre-stable.
- **Implementations it must admit from the start:**

  | Side | Production | Reference |
  |---|---|---|
  | Server | garam's connection service, in `garamsh/garam` | An in-memory server, to be built in `reference/` beside this file |
  | Client | The connector, `cmd/connector` in `garamsh/garam-bot-operator` | The server suite's own client, to be built in `conformance/` beside this file |

  None of the four is built when this version is written.

- **What `v1` needs:** two production server implementations and two production client implementations that pass `conformance.md`.

## Terms

- **Server:** whatever serves this protocol for garam. Where garam serves it is garam's to decide.
- **Client:** a connector. One client is one cluster record of one organization.
- **Cluster record:** the garam resource `grn:<org>/iam/cluster/<id>` that the client authenticates as.
- **Declaration:** one object of a declared kind, written in garam and placed on one cluster record. It has an identifier and a generation.
- **Declared kinds:** the kinds of garam-bot's interface 1 at its `v1alpha1`, Bot, Workspace, WorkspaceClass and BotPool, and no others.
- **Owner:** `garam` or `cluster`, carried on every object by interface 1's managed-by marker. A garam-owned object's marker also names the cluster record that applied it, by GRN, beside the declaration's identifier and generation.
- **Own object:** for a client, a garam-owned object whose marker names its own record, or a cluster-owned object in a namespace whose claim names its release (CLAIM-2).

## Transport

- **T-1.** Every request MUST be made by the client, over HTTPS. The server MUST NOT open a connection to the client.
- **T-2.** The client MUST verify the server against the trust root it was configured with. It MUST NOT trust a root carried in any answer.
- **T-3.** Every path in this document is relative to the base URL the client is configured with. It begins `/cluster-connection/v1alpha1/`.
- **T-4.** Bodies are JSON (`application/json`). Both sides MUST ignore a member they do not know.
- **T-5.** An error answer carries `{"error": <code>, "message": <text>}`. A client MUST act on the HTTP status when it does not know the code.
- **T-6.** On 429 the client MUST wait at least the `Retry-After` the server sent. On 5xx or no answer it MUST wait at least 1 second, doubling to at least 60 seconds, before the same request.

## Registration

`POST /cluster-connection/v1alpha1/registrations`, with no `Authorization` header.

- **REG-1.** The request carries `registrationToken` and `jwk`, the public half of an RSA key of at least 3072 bits as an RFC 7517 JWK. It also carries `connector.version`.
- **REG-2.** The client MUST generate the key itself. It MUST NOT send the private half in this request or any other.
- **REG-3.** A request whose `registrationToken` the server parses spends that token, whatever the outcome.
- **REG-4.** The server MUST answer a spent, expired or unknown token with one answer: 401 `registration_refused`.
- **REG-5.** A token MUST NOT be accepted more than one hour after it was minted.
- **REG-6.** On success the server answers 201 with `cluster` (the record's GRN), `clientId` and `tokenUrl`. From then on the JWK is the record's only live key, and every key the record held before is withdrawn.
- **REG-7.** The client MUST NOT retry a registration with the same token after any answer but 429, or after no answer. It reports the failure and waits for a new token. After a 429 it waits as T-6 says and retries.
- **REG-8.** The server MUST mint a registration token with at least 128 bits from a cryptographically secure random source. It SHOULD bound registration requests per source address. A server that bounds them MUST answer past the bound with 429 before reading the token, so a 429 spends nothing.

## Authentication

- **AUTH-1.** Every request but registration MUST carry `Authorization: Bearer <token>`. The token is one the client exchanged at `tokenUrl`, under garam's `machine-token.v2`, by a `private_key_jwt` assertion signed RS256 with a live key of its record. The header's `kid` is the key's RFC 7638 thumbprint. `iss` and `sub` are `clientId`, and `aud` is `tokenUrl`.
- **AUTH-2.** The server MUST read, on every request, that the token is live, that its cluster record exists and that the key it was exchanged under is live. A request failing any of these MUST be answered 401.
- **AUTH-3.** On a 401 the client MUST exchange once more and repeat the request once. A second 401 is reported and backed off as T-6 says.
- **AUTH-4.** A token of a cluster record MUST reach only this protocol's routes and the bot-token mint garam names for a cluster. Every other machine operation MUST refuse it.
- **AUTH-5.** The bot-token mint MUST refuse a cluster record's token for a bot that is not placed on that record by a live declaration.

## Key rotation

`POST /cluster-connection/v1alpha1/clusters/self/keys`

- **ROT-1.** The request carries `jwk`, the successor's public half, and two compact JWS, each signed RS256 with claims `iss` and `sub` equal to `clientId`, `aud` equal to this route's absolute URL, a fresh `jti`, and `exp` at most 60 seconds after `iat`:
  - `authorization`, signed by a live key of the record. Its header `kid` is that key's thumbprint, its `typ` is `key-rotation+jwt`, and its claim `jkt` is the successor's RFC 7638 thumbprint.
  - `proof`, signed by the successor. Its header `kid` is the successor's thumbprint and its `typ` is `key-proof+jwt`.
- **ROT-2.** The server MUST refuse the request with 400 `invalid_proof` when either JWS fails to verify, names another audience, has expired or repeats a `jti`, when `authorization` is signed by a key that is not live on the record, or when its `jkt` is not the thumbprint of `jwk`. The bearer token alone MUST NOT rotate a key.
- **ROT-3.** On success the server answers 201 with the successor's `kid` and `predecessorRetiresAt`, at least 24 hours away. Until then both keys are live.
- **ROT-4.** A request whose `jwk` is already live on the record MUST be answered as its first success was, with the same `kid` and `predecessorRetiresAt`, provided `authorization` and `proof` verify as ROT-2 requires. A repeat carries freshly signed JWSs, with new `jti` values. Otherwise, when the record already holds two live keys, the server MUST answer 409 `too_many_keys`.
- **ROT-5.** The client MUST store the successor before it sends the request. It signs with the successor once an answer arrives. When no answer arrives, or the answer is 409, it MUST try an exchange with the successor before trying anything else. An exchange that succeeds means the rotation took place. When it fails, the client repeats the rotation with fresh JWSs after a lost answer. After a 409 it keeps signing with its predecessor and reports the failure, since another key it did not send is live on the record.

## Declarations

`GET /cluster-connection/v1alpha1/clusters/self/declarations?after=<cursor>&wait=<seconds>`

- **DECL-1.** `after` is an opaque cursor from an earlier answer. With no `after`, the server answers the full set: every live declaration placed on the record, each as `apply`, with `full: true`.
- **DECL-2.** With `after`, the server answers the changes written after that cursor, in the order they were written. Each change is `apply`, carrying the whole object, or `delete`, a tombstone carrying the identifier.
- **DECL-3.** When a change exists after the cursor, the server MUST answer at once. Otherwise it MUST hold the request up to `wait` seconds and answer as soon as a change is written. `wait` is from 0 to 50 and defaults to 50.
- **DECL-4.** An answer with no change carries the same cursor and an empty `changes`.
- **DECL-5.** An answer MAY carry `more: true`. The client then asks again at once with the new cursor. A full set is complete only at the answer with `more` absent or false.
- **DECL-6.** The server MUST keep a tombstone for at least 30 days. It records, per cluster record, the latest position up to which it has purged tombstones. A cursor at or before that position MUST be answered 410 `cursor_expired`, whether or not any tombstone remains. The client then reads the full set.
- **DECL-7.** A declaration's object carries `apiVersion`, `kind`, `metadata.namespace`, `metadata.name` and `spec`. Its `kind` MUST be a declared kind. The server MUST NOT store or send any other.
- **DECL-8.** A declaration's object MUST NOT carry a secret value. A secret is named only as `<secret-name>/<key>`, in the object's own namespace. The secret's name is a DNS-1123 subdomain, and the key matches `[-._a-zA-Z0-9]+` and is neither `.` nor `..`.
- **DECL-9.** A declaration's object MUST NOT name a container image. Images are the cluster's configuration.
- **DECL-10.** Applying is idempotent by `(id, generation)`. The client MUST NOT apply a generation lower than the one it has applied for that identifier, and MAY skip one it has already applied.
- **DECL-11.** The client applies an `apply` change as an object owned by `garam`, whose marker carries its own record's GRN and the declaration's identifier and generation. It MUST NOT apply one whose namespace and name are held by any object other than its own garam-owned object for that identifier. That covers a cluster-owned object and a garam-owned object of another record. It reports the change as `conflict` instead.
- **DECL-12.** The client MUST NOT apply a change that breaks DECL-7, DECL-8 or DECL-9, and reports it as `invalid`. It also reports a change for a namespace it does not watch as `namespaceNotWatched`.
- **DECL-13.** The client deletes a garam-owned object only on a `delete` change for its identifier. It MUST NOT delete an object because a full set does not name it. It reports such an object as `orphaned` instead.
- **DECL-14.** The client MUST NOT change or delete a cluster-owned object, or a garam-owned object of another record, on any change.
- **DECL-15.** A client configured to report only applies nothing and deletes nothing, and still reports.

## Namespace claims

- **CLAIM-1.** A namespace's claim is a `coordination.k8s.io` Lease named `garam-connector-claim` in it. Its `holderIdentity` names one release of the client, as `<release-namespace>/<release-name>`, the identity the client is configured with. The claim is created and deleted by whatever installs the client: the connector chart renders it, so `helm uninstall` removes it. It has no duration and never expires.
- **CLAIM-2.** The client MUST NOT create, update or delete a claim. It acts in a watched namespace only while that namespace's claim exists and names its own release. In any other watched namespace, it applies nothing, deletes nothing and reports nothing, and lists the namespace as `claimedElsewhere`.
- **CLAIM-3.** The claim coordinates releases the cluster's administrator installed. It is not a security boundary. What keeps one record from changing or reporting another record's garam-owned objects is the record's GRN in their marker (DECL-11, DECL-14, REP-1).

## Reports

`POST /cluster-connection/v1alpha1/clusters/self/reports`

- **REP-1.** A report is the client's whole view, never a delta. It carries:
  - `observedAt`;
  - `connector`, with its `version`, its `mode` (`apply` or `reportOnly`), the `namespaces` whose claim names its release, and the watched namespaces it found `claimedElsewhere`;
  - `declarations`, the outcome of each identifier the client last received: `applied`, `conflict`, `invalid` or `namespaceNotWatched`, with the generation and a message;
  - `objects`, every own object (see Terms) of a declared kind in the namespaces whose claim names its release.
- **REP-2.** Each object carries `apiVersion`, `kind`, `namespace`, `name`, `uid`, `owner`, `generation` and `status`. A garam-owned object also carries its `declarationId` and `orphaned`. A cluster-owned object also carries its `spec`.
- **REP-3.** A report MUST NOT carry a Secret, a ConfigMap, or any object of a kind that is not declared. A secret is named only as DECL-8 names it.
- **REP-4.** The client MUST send a report within 10 seconds of a change it observed, and at least every 5 minutes.
- **REP-5.** The server answers 204 to a report it accepts. It keeps only the report with the latest `observedAt` and ignores an older one. It records the time of every authenticated request as the record's `lastContactAt`.
- **REP-6.** A body over 4 MiB MAY be answered 413 `report_too_large`. The client then reports the error and goes on applying declarations.
- **REP-7.** A report is untrusted input. The server MUST refuse, with 400 `invalid_request`, a report carrying an object of a kind that is not declared, or a Secret or ConfigMap.
- **REP-8.** The server MUST ignore an outcome or an object that names a declaration identifier not placed on the reporting record. It records nothing of either.
- **REP-9.** The server MUST treat a cluster-owned object's `spec` and every `status` and `message` as display data. It never acts on them as a declaration, and it renders them only as text.

## Compatibility

- **Within `v1alpha1`,** a release of either side MAY add an optional member, an error code or an outcome value. It MUST NOT remove or rename a member, or change what one means.
- **A change that breaks the rule above is a new version at a new path,** `v1alpha2` or `v1`. A server serving the new version SHOULD keep serving the earlier one for 30 days.
- **A deprecated member** is marked so in this document with the date it is removed, and is removed only by a new version.
- **Interface 1 versions apart from this protocol.** A new version of interface 1 is carried by a new version of this protocol only if DECL-7's set of kinds changes.

## Open questions

- Whether a cluster-owned bot gets a garam identity, and how.
- An explicit action to adopt or release an object, which changes its owner.
- Sealed static keys sent to the cluster: the owner's decision of 2026-11-21, item 5, deferred past this version.
