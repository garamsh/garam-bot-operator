# Connector

How a user's own cluster is connected to garam: the chart a user installs, the connector program in it, its registration and identity, the declarations it syncs both ways, the rights it holds in the cluster, and what garam owes on its side.

None of it is built yet. This document states what is decided. Goal #436 lists the implementation issues, and each section names the decision it holds.

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
- **The private key never leaves the cluster.** It exists in the connector's process and in one Secret in the release namespace.
- **The connector authenticates as garam's other machine callers do,** by `private_key_jwt` under `machine-token.v2`, through `internal/machinetoken`.
- **It rotates its own key every 30 days.** The rotation call carries an authorization the current key signed, naming the successor, and a proof the successor signed. The bearer token alone rotates nothing. garam's 24-hour rollover holds both keys live. A repeated rotation answers as the first did, and a connector with no answer tries its stored successor.
- **Revocation is garam's**: withdrawing a key or deleting the record refuses the next request. A lost key is registered again under the same record.

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

### Rights in the user's cluster

These follow [ADR 0091](adr/0091-hold-only-namespaced-rights-in-a-users-cluster-open-no-inbound-port-and-keep-secrets-in-it.md).

- **Rights:** Roles and RoleBindings in the release namespace and in each watched namespace, and no ClusterRole at runtime. Caches are restricted to those namespaces. RBAC cannot narrow Secret access below a namespace, so each program can read every Secret of a watched namespace, and a watched namespace holds only the bots' objects and the Secrets they reference.
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
- **Why a key generated in the cluster:** ADR 0088.
- **Why every cluster, aurora included:** the owner's direction of 2026-11-24, recorded in ADR 0093. Its precedents are GitHub Actions runners and Actions Runner Controller, and the Rancher agent. The precedents are the GitHub runner's local RSA key and Open Cluster Management's klusterlet.
- **Why one owner per object:** the 11-21 decision's managed-by (ADR 0089). The precedent is argocd-agent's managed and autonomous modes, chosen here per object.
- **Why two programs:** ADR 0090. ADR 0079's seam keeps identity provisioning outside the manager, and it keeps the key and the right to build workloads in different Pods.
- **Why namespaced rights and no images in declarations:** ADR 0091. They bound what a compromised garam or a stolen cluster token can do in a user's cluster.
- **Why an interface with suites:** ADR 0092. It is the owner's interfaces-first principle and `convention-driven-project`'s interfaces.md.

## Open questions

- **How the manager and the control service reach garam without an issued key.** aurora places their keys by hand today, under ADR 0072. ADR 0093 decision 5 keeps those keys in place until an ADR moves both processes onto the registration path.
- **A cluster-owned bot's garam identity.** Does it get a GRN and tokens, and on whose authority?
- **Adoption.** An explicit action that moves an object between `garam` and the cluster as owner.
- **Several bots in one pod.** The 11-21 decision's one pod per organization, with a process per bot, waits for a garam-bot runtime that runs several bots. This goal runs one Bot per StatefulSet.
- **Sealed static keys.** The owner's decision of 2026-11-21, item 5, deferred past the first goal on 2026-11-24.
- **Where users pull the chart and the images from.** Today's repositories are private ECR (`delivery.md`), which a user's cluster cannot pull from.
- **How garam-bot's interface 1 kinds are served here.** `stack-kubebuilder.md` §1 says this project is single-group, and interface 1's group is an open question in `garamsh/garam-bot#1193`.
