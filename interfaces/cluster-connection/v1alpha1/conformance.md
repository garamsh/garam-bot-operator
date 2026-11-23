# cluster-connection v1alpha1 conformance

The tests an implementation of `README.md` must pass, each named and mapped to the requirements it holds. Every requirement there appears here, except two that no black-box test observes on its own. T-1's server half is one. T-3, the path prefix, is the other, and every test exercises it.

## Suites

There are two suites. Each reaches an implementation only through the protocol and the fixture named below, never through its internals.

| Suite | Runs against | Fixture the implementation supplies |
|---|---|---|
| Server | A server's base URL and its trust root | A command the suite runs with one subcommand per action. Each prints JSON: `mint-token`, `declare <file>`, `tombstone <id>`, `latest-report`, `revoke-keys`, `delete-cluster`, `expire-cursors`. garam implements it over its own API. The reference server implements it in memory. |
| Client | The client under test, started by the suite against the reference server and a Kubernetes API server carrying interface 1's CRDs | A command line the suite fills in with the base URL, trust file, registration-token file, release namespace and watched namespaces |

The suites are built under `conformance/` beside this file, as tests that run against a binary or a URL. Until they are, every test below is pending. The conformance runner reports pending tests by name.

## Server suite

| Test | Holds |
|---|---|
| `server/registration/accepts-fresh-token` | REG-1, REG-6 |
| `server/registration/refuses-spent-token` | REG-3, REG-4 |
| `server/registration/refuses-unknown-token-same-as-spent` | REG-4 |
| `server/registration/refuses-expired-token` | REG-4, REG-5 |
| `server/registration/spends-token-on-refused-key` | REG-3 |
| `server/registration/refuses-private-jwk-members` | REG-2 |
| `server/registration/withdraws-earlier-keys` | REG-6 |
| `server/registration/distinct-tokens-are-unpredictable` | REG-8 |
| `server/registration/throttle-spends-nothing` | REG-8 |
| `server/auth/refuses-missing-token` | AUTH-1, AUTH-2 |
| `server/auth/refuses-after-cluster-deleted` | AUTH-2 |
| `server/auth/refuses-after-key-revoked` | AUTH-2 |
| `server/auth/token-reaches-no-other-operation` | AUTH-4 |
| `server/auth/mint-only-bots-placed-on-record` | AUTH-5 |
| `server/rotation/accepts-proved-successor` | ROT-1, ROT-3 |
| `server/rotation/refuses-bad-proof` | ROT-2 |
| `server/rotation/refuses-replayed-proof` | ROT-2 |
| `server/rotation/refuses-rotation-without-current-key-signature` | ROT-2 |
| `server/rotation/refuses-authorization-for-another-successor` | ROT-2 |
| `server/rotation/refuses-authorization-by-retired-key` | ROT-2 |
| `server/rotation/repeat-answers-first-success` | ROT-4 |
| `server/rotation/refuses-third-key` | ROT-4 |
| `server/rotation/predecessor-live-24h` | ROT-3 |
| `server/declarations/full-set-without-cursor` | DECL-1, DECL-5 |
| `server/declarations/changes-in-write-order` | DECL-2 |
| `server/declarations/answers-at-once-when-pending` | DECL-3 |
| `server/declarations/wakes-held-poll-on-write` | DECL-3 |
| `server/declarations/empty-answer-keeps-cursor` | DECL-4 |
| `server/declarations/tombstone-carries-id` | DECL-2 |
| `server/declarations/expired-cursor-is-410` | DECL-6 |
| `server/declarations/expired-cursor-is-410-with-no-tombstone-left` | DECL-6 |
| `server/declarations/refuses-undeclared-kind` | DECL-7 |
| `server/declarations/refuses-secret-value` | DECL-8 |
| `server/declarations/refuses-image` | DECL-9 |
| `server/reports/accepts-report` | REP-1, REP-5 |
| `server/reports/keeps-latest-observed` | REP-5 |
| `server/reports/records-last-contact` | REP-5 |
| `server/reports/refuses-secret-kind` | REP-7 |
| `server/reports/refuses-undeclared-kind` | REP-7 |
| `server/reports/ignores-foreign-declaration-id` | REP-8 |
| `server/reports/spec-is-never-a-declaration` | REP-9 |
| `server/transport/ignores-unknown-members` | T-4 |
| `server/transport/error-body-shape` | T-5 |

## Client suite

| Test | Holds |
|---|---|
| `client/registration/generates-key-sends-public-half` | REG-1, REG-2 |
| `client/registration/no-retry-after-refusal` | REG-7 |
| `client/registration/no-retry-after-no-answer` | REG-7 |
| `client/transport/verifies-configured-root` | T-2 |
| `client/transport/ignores-unknown-members` | T-4 |
| `client/transport/backs-off` | T-6 |
| `client/transport/honours-retry-after` | T-6 |
| `client/transport/unknown-error-code-by-status` | T-5 |
| `client/registration/retries-after-429` | REG-7 |
| `client/auth/re-exchanges-once-on-401` | AUTH-1, AUTH-3 |
| `client/rotation/signs-authorization-with-current-key` | ROT-1 |
| `client/rotation/stores-successor-before-sending` | ROT-5 |
| `client/rotation/signs-with-successor-after-answer` | ROT-5 |
| `client/rotation/tries-successor-after-lost-answer` | ROT-5 |
| `client/rotation/tries-successor-after-409` | ROT-5 |
| `client/declarations/applies-full-set` | DECL-1, DECL-11 |
| `client/declarations/waits-for-more-false` | DECL-5 |
| `client/declarations/rereads-on-410` | DECL-6 |
| `client/declarations/ignores-lower-generation` | DECL-10 |
| `client/declarations/marks-own-record` | DECL-11 |
| `client/declarations/refuses-name-held-by-cluster` | DECL-11 |
| `client/declarations/refuses-name-held-by-another-record` | DECL-11, DECL-14 |
| `client/declarations/refuses-invalid` | DECL-12 |
| `client/declarations/reports-unwatched-namespace` | DECL-12 |
| `client/declarations/deletes-only-on-tombstone` | DECL-13 |
| `client/declarations/reports-orphan` | DECL-13 |
| `client/declarations/leaves-cluster-owned-alone` | DECL-14 |
| `client/declarations/report-only-applies-nothing` | DECL-15 |
| `client/claims/creates-and-renews-claim` | CLAIM-1 |
| `client/claims/stays-out-of-namespace-claimed-elsewhere` | CLAIM-2 |
| `client/reports/whole-view` | REP-1, REP-2 |
| `client/reports/leaves-out-other-records-objects` | REP-1, CLAIM-2 |
| `client/reports/no-secret-or-configmap` | REP-3 |
| `client/reports/within-10s-of-change` | REP-4 |
| `client/reports/at-least-every-5m` | REP-4 |
| `client/reports/goes-on-after-413` | REP-6 |
| `client/transport/never-listens` | T-1 |

`client/transport/never-listens` checks that the client's Pod opens no listening socket other than its probes.
