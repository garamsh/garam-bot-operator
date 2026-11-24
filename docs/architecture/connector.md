# Connector

How a user's own cluster is connected to garam: the chart a user installs, the connector program in it, its registration and identity, the declarations it syncs both ways, the rights it holds in the cluster, and what garam owes on its side.

Registration and identity are built, in `cmd/connector` and `internal/clusteridentity` (#440), except registering again after a key is lost (#450). Applying declarations is built, in `internal/declaration` (#441). The rest is not built yet. This document states what is decided. Goal #436 lists the implementation issues, and each section names the decision it holds.

## Current decisions

### The chart and its two programs

- **A user connects a cluster by installing the connector chart in it**, in EKS or any conformant cluster ([ADR 0087](adr/0087-connect-a-users-cluster-through-a-connector-it-installs-that-dials-garam-over-https-long-poll.md)). The chart is a product users install: for it, ADR 0079's "runs only on our own system" and "BYO on hold" no longer hold.
- **The chart deploys two programs as two Deployments from two images** ([ADR 0090](adr/0090-run-the-connector-as-a-binary-beside-the-manager-and-build-the-bot-reconciler-from-the-agent-workload.md)):
  - **The manager** reconciles garam-bot's interface-1 objects into workloads. It needs no garam connection, so the chart with the connector off is the cluster mode that works without garam.
  - **The connector**, `cmd/connector`, holds the cluster's identity and is the only program that talks to garam.
  - **They meet only through the Kubernetes API.**
- **The Bot reconciler is built from the Agent workload code.** What the two share moves out of `internal/controller` into a package below both. One Bot is one StatefulSet of one, whose runtime reads that Bot from a mounted file.
- **The manager starts a reconciler for each kind the cluster serves**, found by discovery at start. The chart installs no `Agent` CRD, so the chart's manager runs no `Agent` reconciler. No mode flag is added. Where `--control-address` is set, a missing `Agent` kind fails the start.
- **The control-service path is not part of the chart.** `internal/desired`, the `Agent` refresher, the `Agent` kind and the control service stay as `agent.md` and `control.md` describe them, and do not run in a user's cluster.

### The connection

- **Every connection is opened by the connector, outbound, over HTTPS to one garam address.** garam never dials the cluster ([ADR 0087](adr/0087-connect-a-users-cluster-through-a-connector-it-installs-that-dials-garam-over-https-long-poll.md)).
- **Declarations arrive by long-poll, and reports leave by POST.** garam answers an open poll as soon as a change is written, and otherwise after at most 50 seconds.
- **The wire is `interfaces/cluster-connection/v1alpha1/`** ([ADR 0092](adr/0092-specify-the-garam-connector-boundary-as-the-cluster-connection-interface-with-conformance-suites.md)): the normative specification, its OpenAPI schemas, and the conformance tests mapped to its requirements. garam implements the server, and an in-memory reference server here is the second implementation.

### Registration and identity

These follow [ADR 0088](adr/0088-register-a-connector-with-a-one-time-token-and-a-key-it-generates-in-the-cluster.md), and the rule over them is [ADR 0093](adr/0093-connect-every-cluster-aurora-included-by-a-registration-token-and-keys-it-generates-and-renews.md).

- **Every cluster connects this way, aurora included.** Its only credential at install is a registration token given once. Its key is generated in the process that holds it, kept there and in one Secret, and renewed without a person. No key is issued by garam or placed by hand.
- **A cluster is a garam resource, `grn:<org>/iam/cluster/<id>`, in one organization.** One chart release is one record. A second organization on the same cluster is a second release in another namespace.
- **Registration spends a one-time token.** The token lives one hour, is good for one attempt, and garam keeps only its hash. The connector generates an RSA 3072 key itself and sends the public JWK with the token.
- **The private key never leaves the cluster.** It exists in the connector's process and in one Secret in the release namespace, which `--identity-secret` names. That Secret also holds the record, the successor while a rotation is unresolved, and the SHA-256 of the registration token last presented, never the token. The connector never writes the key to a file, and signs each exchange with the key it holds through `internal/machinetoken`'s `Identity`. It writes the Secret only on the `resourceVersion` it last read, so it never writes over another process: on a conflict it stops, and its restart reads what the other wrote.
- **A registration token is presented once.** The connector records the attempt before it sends the token. After any answer but 429, or after none, it waits for a different token in its token file, across restarts too (REG-7).
- **The connector authenticates as garam's other machine callers do,** by `private_key_jwt` under `machine-token.v2`, through `internal/machinetoken`.
- **It rotates its own key every 30 days.** The rotation call carries an authorization the current key signed, naming the successor, and a proof the successor signed. The bearer token alone rotates nothing. garam's 24-hour rollover holds both keys live. A repeated rotation answers as the first did, and a connector with no answer tries its stored successor.
- **Revocation is garam's**: withdrawing a key or deleting the record refuses the next request. A lost key is registered again under the same record.
  - **Registering again is not built yet** ([ADR 0088](adr/0088-register-a-connector-with-a-one-time-token-and-a-key-it-generates-in-the-cluster.md) decision 7, #450). The connector reads its token file only while its Secret holds no record, so one whose key garam no longer accepts backs off its rotation without end and ignores a new token.
  - **Until it is, it is recovered by hand:** empty the identity Secret's data (`kubectl -n <release-namespace> patch secret <identity-secret> --type=json -p '[{"op":"remove","path":"/data"}]'`), place a new registration token in the token file, and restart the connector. The Secret itself stays, because the connector never creates one.

### Declarations and ownership

These follow [ADR 0089](adr/0089-sync-declarations-both-ways-and-give-each-object-one-owner.md).

- **Each object of a declared kind has one owner, `garam` or the cluster**, recorded by interface 1's managed-by marker. A garam-owned object's marker also names the cluster record that applied it.
- **Each watched namespace is claimed by one release**, through a Lease the chart renders naming the release. It never expires, and `helm uninstall` removes it. Helm refuses a second release's claim, and both programs act only under their own release's claim. The claim coordinates. The record's GRN in each object's marker is what isolates one record's objects from another's.
- **garam-owned objects are applied by server-side apply, and drift in them is reverted.**
- **Cluster-owned objects are reported to garam and are read-only there.**
- **A collision is refused, never overwritten.** A name held by a cluster-owned object, or by another record's object, makes the garam declaration a `conflict`.
- **Deletion is by explicit tombstone only.** garam records how far it has purged tombstones, and a cursor at or before that point reads the full set again. An object the full set no longer names is reported `orphaned` and left in place.
- **Losing garam changes nothing in the cluster.**
- **A declaration carries secrets only as `<secret-name>/<key>` references, and names no image.**

How the connector applies them (#441):

- **A pipeline.** The connector long-polls the declarations and holds the latest generation of each. A full set replaces what it holds only at its last page. A 410 reads the full set again. A pass reads the watched namespaces' claims and their objects, decides, and writes. It runs on every change of the declarations held, on every change of an object of a declared kind or of a claim, every 10 minutes, and again after a back-off when it failed. No pass runs before the first full set has been read.
- **What it checks before applying.** Interface 1's own parser, `manifest.Parse` from `github.com/garamsh/garam-bot/api/declaration/v1alpha1`, admits an object as the API server would under strict field validation: a declared kind, secrets by reference alone, and no field naming an image. The object also needs `metadata.namespace`. A declaration that fails any of these is `invalid`, and so is one the API server refuses.
- **What is applied.** The declaration's `apiVersion`, `kind`, namespace, name, labels, annotations and `spec`, with the ownership marker naming the record, the identifier and the generation. Nothing else of the object is applied.
- **How it writes.** It writes under one field manager, `garam-connector`. An object is created where nothing holds its name. Every later write is a server-side apply that forces the fields a hand edit took, which reverts the edit. That apply is made on the `resourceVersion` the pass read, so an object changed since is decided again by the next pass rather than written over. The first apply after a create hands the create's fields to the apply's entry, so a field a later generation drops is removed. A delete carries the object's uid and `resourceVersion` as preconditions.
- **Each declaration's outcome** is kept for the reports (#442):
  - `applied` where the object is the record's for that identifier, at the generation applied;
  - `conflict` where another object holds the name: a cluster-owned object, another record's, the record's own object for another declaration, or an object whose marker is malformed;
  - `invalid`, as above;
  - `namespaceNotWatched`.

  A declaration for a namespace claimed elsewhere has no outcome, because nothing is reported there. In report-only mode, a declaration is `applied` only where the cluster already holds that generation, and otherwise has no outcome.
- **An orphan** is one of the record's objects that no declaration held names, at the namespace and name its latest generation names.
- **A tombstone** deletes the record's objects for its identifier, in the claimed namespaces. A full set read after it drops it, so what it named and did not delete is reported `orphaned`.

### Rights in the user's cluster

These follow [ADR 0091](adr/0091-hold-only-namespaced-rights-in-a-users-cluster-open-no-inbound-port-and-keep-secrets-in-it.md).

- **Rights:** Roles and RoleBindings in the release namespace and in each watched namespace, and no ClusterRole at runtime. Caches are restricted to those namespaces. RBAC cannot narrow Secret access below a namespace, so each program can read every Secret of a watched namespace, and a watched namespace holds only the bots' objects and the Secrets they reference.
- **The identity Secret is the chart's to create, empty, and the connector's to write.** The connector never creates a Secret, because RBAC cannot narrow `create` by name. Its Role grants `get` and `update` on that one Secret by `resourceNames`, and no `create`, `list` or `watch`. The chart never writes the Secret's data after creating it, so an upgrade keeps the key. The chart is #445's.
- **The connector's Role in each watched namespace** grants `get`, `list`, `watch`, `create`, `patch` and `delete` on interface 1's four kinds, and `get`, `list` and `watch` on Leases. Server-side apply, and the JSON patch that hands a create's fields to the apply's entry, are both `patch`. Its cache holds only the claim, by a field selector on the Lease's name, but RBAC cannot narrow `list` or `watch` by name. It never writes a Lease (CLAIM-2). The chart is #445's.
- **CRDs:** installed by Helm with the installer's rights, and the only cluster-scoped objects the chart renders.
- **No inbound path:** no LoadBalancer or NodePort Service, no Ingress, no webhook. A NetworkPolicy admits no ingress. Egress goes to the configured proxy, or else to garam on 443, plus DNS, plus the API server's range given as a value.
- **Metrics:** off by default, and plain HTTP inside the cluster when turned on.
- **Secrets:** no Secret or ConfigMap is ever reported, and no credential reaches a log, metric, event, status or report.
- **Images:** pinned by digest in the chart. A user can set the connector to report only.

## What garam owes

These are obligations on `garamsh/garam`, for that repository to decide and build. The interface is the part both sides share. The rest is garam's alone.

| Obligation | From |
|---|---|
| Serve `interfaces/cluster-connection/v1alpha1/` and pass its server suite in CI | ADR 0092 |
| A Cluster resource, `grn:<org>/iam/cluster/<id>`, with browser routes to create, read, rename, describe and delete it, and to revoke its keys | ADR 0088 |
| Mint a one-time registration token on creation and on re-registration, under a new garam ADR superseding garam ADR-0117 in part | ADR 0088 |
| Store what ADR 0088 decision 8 lists, and nothing it excludes | ADR 0088 |
| `iam:cluster.register`, `.read`, `.update`, `.reissue` and `.delete` | ADR 0088 |
| Write a bot or workspace declaration placed on a cluster and namespace, with an identifier, a generation and a tombstone on delete | ADR 0089 |
| Refuse a declaration that names an image, carries a secret value, or takes a name a cluster-owned object holds in the latest report | ADR 0089, ADR 0091 |
| Show a cluster's reported objects, read-only where cluster-owned, with each declaration's outcome and `lastContactAt` | ADR 0089 |
| Let a cluster's token mint the garam tokens of a garam-owned bot placed on that cluster by a live declaration, and no other bot's, and reach no other operation (AUTH-4, AUTH-5) | ADR 0090, ADR 0088 |
| Treat a report as untrusted input: refuse undeclared kinds, ignore what names another record's declarations, and render cluster-owned specs as text only (REP-7 to REP-9) | ADR 0089 |

## Rationale

- **Why a connector rather than garam reaching the cluster:** the decision of 2026-11-24 (`garamsh/naru#373` §3) and ADR 0087's precedents.
- **Why long-poll:** ADR 0087's comparison. gRPC streams and WebSocket tunnels need a sticky replica, and a tunnel opens a path into the cluster.
- **Why a key generated in the cluster:** ADR 0088. The precedents are the GitHub runner's local RSA key and Open Cluster Management's klusterlet.
- **Why every cluster, aurora included:** the owner's direction of 2026-11-24, recorded in ADR 0093. Its precedents are GitHub Actions runners and Actions Runner Controller, and the Rancher agent.
- **Why one owner per object:** the 11-21 decision's managed-by (ADR 0089). The precedent is argocd-agent's managed and autonomous modes, chosen here per object.
- **Why two programs:** ADR 0090. ADR 0079's seam keeps identity provisioning outside the manager, and it keeps the key and the right to build workloads in different Pods.
- **Why namespaced rights and no images in declarations:** ADR 0091. They bound what a compromised garam or a stolen cluster token can do in a user's cluster.
- **Why an interface with suites:** ADR 0092. It is the owner's interfaces-first principle and `convention-driven-project`'s interfaces.md.

## Open questions

- **How the manager and the control service reach garam without an issued key.** Asked in `configuration.md` §Open questions, which describes those two processes' keys.
- **A cluster-owned bot's garam identity.** Does it get a GRN and tokens, and on whose authority?
- **Adoption.** An explicit action that moves an object between `garam` and the cluster as owner.
- **Several bots in one pod.** The 11-21 decision's one pod per organization, with a process per bot, waits for a garam-bot runtime that runs several bots. This goal runs one Bot per StatefulSet.
- **Sealed static keys.** The owner's decision of 2026-11-21, item 5, deferred past the first goal on 2026-11-24.
- **Where users pull the chart and the images from.** Today's repositories are private ECR (`delivery.md`), which a user's cluster cannot pull from.
- **How garam-bot's interface 1 kinds are served here.** `stack-kubebuilder.md` §1 says this project is single-group, and interface 1's group is an open question in `garamsh/garam-bot#1193`.
