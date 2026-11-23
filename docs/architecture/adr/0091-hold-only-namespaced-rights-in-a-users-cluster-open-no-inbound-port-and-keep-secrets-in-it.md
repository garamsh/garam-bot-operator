# ADR 0091: Hold only namespaced rights in a user's cluster, open no inbound port, and keep secrets in the cluster

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The connector runs in a cluster garam does not own (ADR 0087). This goal's brief asks for three things: least privilege in the user's cluster, namespaced by default; no inbound port; and secrets staying in the user's cluster. The owner decided on 2026-11-24 that there is a namespaced Role, optional watched namespaces, and no runtime ClusterRole.

Today the manager deployed from `config/` holds a ClusterRole, `config/rbac/role.yaml`. Its metrics endpoint is guarded by an authentication filter that needs `tokenreviews` and `subjectaccessreviews`, which are cluster-scoped (`config/rbac/metrics_auth_role.yaml`, a ClusterRole). The deployment in our own system keeps that shape. This ADR governs the connector chart only.

The 11-21 decisions (`garamsh/discussions` discussion 6, comments 19791278 and 19791501) also say:

- A secret never enters a workspace.
- A `secretRef` tool key is a cluster Secret whose value garam never sees.
- A static key typed into the dashboard is sealed to the cluster's public key, in the style of Sealed Secrets. The owner deferred that past this goal on 2026-11-24.

## Decision

1. **The chart's runtime rights are Roles in named namespaces, and no ClusterRole.**
   - The release namespace always holds the leader-election Lease, the cluster key's Secret and the two programs' own objects.
   - The watched namespaces are a chart value, defaulting to the release namespace alone. The chart renders a Role and a RoleBinding in each one for each program.
   - Both programs restrict their caches to those namespaces, so neither lists or watches anything cluster-wide.
   - Both act only in a watched namespace whose claim (ADR 0089 decision 8) names their release: the cluster record, or, with the connector off, the release itself. Two releases watching one namespace therefore leave it to whichever claimed it first.

2. **Each program holds only the verbs its own work calls.**

   | Program | Rights |
   |---|---|
   | Manager | The workload objects it builds, in watched namespaces: StatefulSets, claims, Pods and events. Listing and watching Secrets there, to learn whether a referenced one exists. Writing interface-1 objects' status. The claim Lease in each. |
   | Connector | Writing interface-1 objects, in watched namespaces. Creating Secrets there, and getting and patching them, for the bot credential Secrets it writes. Reading and writing its own key Secret, by name, in the release namespace. The claim Lease in each. |

   RBAC cannot narrow either further. A Role grants `list` and `watch` on every Secret of its namespace or on none. `resourceNames` cannot bound `create`, and cannot name a bot's credential Secret before the bot exists. So in a watched namespace each program can read every Secret. A watched namespace is one that holds the bots' objects and the Secrets they reference, and nothing else the user would not hand to these two programs. The chart's documentation says so.

3. **CRDs are installed by Helm, with the installer's rights, and are the only cluster-scoped objects the chart renders.** A user whose cluster already holds them installs with `--skip-crds`. Nothing at runtime creates or changes a CRD.

4. **The chart opens no inbound path.**
   - It renders no Service of type LoadBalancer or NodePort, no Ingress and no Gateway route, and no admission webhook.
   - Each program's Pod is selected by a NetworkPolicy that admits no ingress.
   - The connector's egress goes to the configured forward proxy where one is set, and otherwise to garam's address on 443, plus cluster DNS.
   - Egress to the Kubernetes API is the address range the user gives as a chart value. Where that address lies is the distribution's, and no portable selector names it.
   - Metrics are off by default. When turned on they are plain HTTP inside the cluster, admitted by a NetworkPolicy rule the user names, because the authenticated metrics filter needs the ClusterRole decision 1 refuses.

5. **Secrets stay in the cluster.**
   - garam sends no secret value. A declaration names a secret only as `<secret-name>/<key>` in the object's namespace (ADR 0089).
   - The connector reports no Secret, and no ConfigMap. A report carries only objects of interface-1 kinds, whose specs carry references.
   - The cluster's private key exists in the connector's process and in one Secret in the release namespace, readable by the connector alone (ADR 0088).
   - No key, token, assertion or registration token reaches a log line, a metric, an event, a status or a report.

6. **A declaration names no container image, and the connector refuses one that does.** Which images run is the chart's configuration, pinned by digest. So someone holding garam, or holding the cluster's token, can declare bots and workspaces in watched namespaces, and run nothing the cluster's own administrator has not installed. A user can also set the connector to report only, in which case it applies nothing garam sends.

7. **Both programs run as a PodSecurity `restricted` namespace admits them**, as the Agent workload does (`agent.md`). Every image of the chart is pulled by digest.

## Consequences

- garam's blast radius in a user's cluster is the declared kinds, in the namespaces the user listed, run with the images the user installed. Through the protocol, garam cannot exec into Pods, read Secrets or reach the API: the connector sends no Secret and acts only on declared kinds.
- Someone running code inside the connector's or the manager's Pod can read the Secrets of the watched namespaces, by the rights above. That is why a watched namespace holds nothing else.
- Watching a new namespace is a chart upgrade, so it is a change the cluster's administrator makes.
- A cluster without NetworkPolicy enforcement keeps every other guarantee here. The NetworkPolicy is a second layer, and the chart's documentation says so.
- Sealed static keys are not built in this goal. Building them later adds a sealing key that only decrypts and never signs, a key separate from the one in ADR 0088.

## Signs the decision was wrong

- Users need the connector in so many namespaces that the per-namespace Roles become the friction, and they ask for a ClusterRole.
- A declared kind cannot do its work without an image chosen per bot, which would show that decision 6's closed set of images does not fit the runtime.
- Secret references that cross namespaces become necessary, and they would need rights this ADR does not grant.

## Rejected alternatives

- **One ClusterRole, as the manager in `config/` holds.** It is simpler to install, but every namespace and every Secret in the cluster would be in reach.
- **Letting garam send Secret values, encrypted in transit.** garam would hold or relay the value. ADR 0036 at garam keeps a secret in garam as a reference, never a value, and the sealed path is the owner's answer for the one case that needs a value.
- **An exposed metrics Service.** It is an inbound path, and the scraper can reach the Pod inside the cluster.
