# ADR 0088: Register a connector with a one-time token and a key it generates in the cluster, and authenticate as a cluster garam records

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

`garamsh/naru#373` §3 says the connector registers with a one-time token. This goal's brief asks for a one-time token followed by a workload credential that is rotated and revocable. The owner settled the shape on 2026-11-24, answering this goal's questions (relayed by cupa): a one-time registration token, a key generated in the cluster, `private_key_jwt`, rotation signed by the current key, a new garam resource for the cluster, and one install per cluster record per organization.

What garam has today, at `garamsh/garam@6c3aaf69ed5a76bf4451353371f99a9d2a390222`:

- **An operator enters by a key garam generates.** `issueOperatorKey` answers an RSA 3072 private key once as PEM, and a person places it (`docs/architecture/operators.md` §Issued PEM identity). The key exists in garam's process and in the answer to the browser.
- **The key is exchanged for one-hour tokens** by `private_key_jwt` under `machine-token.v2` at `POST /oauth/token` (garam ADR-0102). A token is read on every request together with the registration and the live key, so deleting the record or withdrawing the key refuses the next request (garam ADR-0070, ADR-0100).
- **Planned rotation holds two live keys for 24 hours**, and `immediate` withdraws every live key at once (`operators.md` §Issued PEM identity).
- **The one-time enrollment token is gone.** garam ADR-0064 had an operator generate its own key and enroll with a one-time token. ADR-0117 removed it because no live deployment used it. This repository's half was ADR 0020 to ADR 0022, removed by ADR 0072.
- **Clusters are not a resource.** `docs/architecture/overview.md` §Out of scope lists "Clusters as a resource an organization connects" as named and unplanned.

Precedents for the same shape:

- **GitHub Actions self-hosted runners.** The registration token "expires after one hour" (docs.github.com/en/rest/actions/self-hosted-runners, read 2026-11-24). At configuration "an RSA public/private key pair is created, the private key is stored in file on disk". The runner sends only the public key. At start it asks the token service for an OAuth token "signed with the RSA private key" (`actions/runner` `docs/design/auth.md`, read 2026-11-24).
- **Open Cluster Management.** The klusterlet joins with a bootstrap hub token, and its CSR stays pending until a hub administrator accepts it. Its private key "is stored in each managed cluster, and it will not be transmitted across the network" (open-cluster-management.io, Register a cluster, read 2026-11-24).

## Decision

1. **A cluster is a garam resource of its own, `grn:<org>/iam/cluster/<id>`, in one organization.** It sits under `iam` because garam's catalogue places every machine identity there, `operator`, `supervisor` and, by garam ADR-0179, `bot` (`docs/architecture/grn-model.md` §Types by owning service). The record is the connector's machine identity, and it is the target that declarations are placed on (ADR 0089). It is not an Operator record: the Operator record carries hosted-operator delegation, grants and a control endpoint, and none of those applies to a connected cluster.

2. **One chart release is one cluster record in one organization.** A second organization connecting the same physical cluster installs a second release, in another namespace, and registers a second record. A release never serves two organizations. This follows the 11-21 decision that organizations are not mixed. Two releases never act in the same namespace: each holds a claim on each namespace it watches, and stays out of one another release has claimed (ADR 0089).

3. **Registration is one call that spends a one-time token.**
   - A member with `iam:cluster.register` creates the record from the dashboard, and garam answers a registration token once.
   - The token lives one hour and is good for one attempt. Presenting it spends it whatever the outcome, so a spent, expired or unknown token gets one answer.
   - garam stores only the token's hash, and deletes the hash once it is spent or expired.
   - The connector generates its key in its own process and sends the token with the public JWK. garam binds that key to the record, and the answer carries the record's GRN, the `client_id` and the `tokenUrl`.
   - The call presents no other credential, and the connector verifies garam against the server root the chart was given, never against anything the answer carries (as ADR 0020 decided for the earlier enrollment).

4. **The private key never leaves the cluster.** The connector writes it to one Secret in its release namespace, and nothing else reads it (ADR 0091). This is the reason for this design over garam's issued PEM, where the key passes through garam's process and a browser. It is also why garam brings the one-time token back, by a new garam ADR that supersedes garam ADR-0117 in part.

5. **After registration, the connector authenticates as garam's other machine callers do.** It exchanges a `private_key_jwt` assertion for a one-hour token under `machine-token.v2`, and presents the token on every call. `internal/machinetoken` already implements that client (ADR 0072).

6. **The connector rotates its own key, and signs the rotation with the current one.** It generates a successor and sends its public JWK on a call that carries three things:
   - the current token;
   - an authorization the current key signed, naming the successor's thumbprint;
   - a proof the successor signed.

   The bearer token alone rotates nothing, so a token that leaks, through a TLS-inspecting proxy or a log, cannot install another key. garam holds the two keys live for its existing 24-hour rollover, after which the predecessor is withdrawn, and a third live key is refused. Repeating a rotation whose answer was lost answers as the first did. A connector that got no answer, or a 409, tries its stored successor at the exchange before anything else. The connector rotates every 30 days, and on a person's request through the chart. The wire is ROT-1 to ROT-5.

7. **Revocation is garam's and takes effect on the next request.** An administrator withdraws a key or deletes the cluster record. garam reads both on every request, as it does for an operator. A cluster that lost its key is registered again with a new token, under the same record (`iam:cluster.reissue`). That withdraws every key the record held.

8. **What garam stores for a cluster** is listed below. It stores no private key, no Secret value, no Kubernetes credential and no address of the cluster.

   | Stored | Kept until |
   |---|---|
   | The record: GRN, organization, name, description, who created it and when | The record is deleted |
   | A registration token's hash and expiry | The token is spent or expires |
   | Each public JWK with its RFC 7638 thumbprint and state (active, retiring, revoked) | The record is deleted |
   | Token hashes, as for an operator | The token expires or its key is withdrawn |
   | Declarations and their tombstones (ADR 0089), and how far tombstones have been purged | Tombstones are kept at least 30 days |
   | The latest report: the cluster's objects of the declared kinds, the watched namespaces, the connector's version, `lastContactAt` | Replaced by the next report |

9. **The IAM actions are named the way garam names the operator's.** `iam:cluster.register`, `iam:cluster.read`, `iam:cluster.update` (name and description), `iam:cluster.reissue` and `iam:cluster.delete`. Which roles hold each is garam's to decide.

## Consequences

- garam carries a new resource, a registration route on its machine listener that takes no token, a key-rotation route, and the browser routes above. `interfaces/cluster-connection/v1alpha1/` specifies the machine-listener half, and garam's new ADR the rest.
- A lost key costs a re-registration and nothing else. Declarations are the record's, not the key's, so they are served again to the re-registered connector.
- garam can bound what a cluster record reaches by the record alone: its token reaches the cluster-connection routes and the bot-token mint (ADR 0090), and no other operation.
- A user's installation now has two steps: create the record and copy the token, then install the chart with it. That is the GitHub runner's shape.

## Signs the decision was wrong

- Users lose cluster keys often enough, by deleting the namespace or restoring a backup without Secrets, that re-registration is routine rather than exceptional.
- A cluster's connector has to act for more than one organization, and installing one release per organization proves too heavy.
- garam's operator and cluster records grow the same features one by one, which would show they were one kind.

## Rejected alternatives

- **garam's issued PEM.** It needs no new credential kind, but the private key passes through garam and the browser. The owner chose the key generated in the cluster for that reason.
- **Registering the connector as an Operator.** Decision 1.
- **A Kubernetes ServiceAccount token from the cluster's OIDC issuer, trusted by garam as a federated identity.** It needs no stored key. But the cluster's issuer must be reachable from garam, which EKS offers and a kind cluster or a private cluster does not.
- **An administrator approving a key the connector shows, as garam's standalone host approval does (garam ADR-0082).** It is a second step in the dashboard that the token makes unnecessary, since the token is minted by the same administrator.
