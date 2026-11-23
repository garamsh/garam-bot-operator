# ADR 0089: Sync declarations both ways between garam and a connected cluster, and give each object one owner

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The owner's decision of 2026-11-21 (`garamsh/discussions` discussion 6, comment 19791278, item 4) gives three ways to declare a bot or a workspace: a file, a CRD, and garam's dashboard or API. It gives each object one owner, recorded as managed-by. The decision of 2026-11-24 (`garamsh/naru#373` §3) has garam send the user's declarations to the connector, which reconciles them as CRDs in the cluster. This goal's brief adds the reverse direction: a declaration made in the cluster is reported to garam, so the cluster works without garam and garam still shows it.

The kinds are garam-bot's interface 1, the declarative spec: Bot, Workspace, WorkspaceClass and BotPool. That specification is `garamsh/garam-bot#1193`, and it is not yet written.

What this repository already does with the same question:

- **A hand-written object is never taken over.** The renderer refuses an `Agent` that carries no identity or another GRN under a name the feed wants, and leaves it as it is (`ErrNotControlSource`, `agent.md`).
- **Absence deletes nothing.** An agent the control feed stops naming keeps its `Agent` (`agent.md`, "Withheld agents").

Precedent: argocd-agent has two agent modes. In managed mode "the control plane cluster is responsible for maintaining the configuration of an agent". In autonomous mode "the workload cluster is wholly responsible for maintaining its own configuration" (argocd-agent.readthedocs.io, Agent modes, read 2026-11-24). This decision makes that choice per object rather than per cluster.

## Decision

1. **Every object of a declared kind has exactly one owner, `garam` or the cluster**, recorded by interface 1's managed-by marker. An object the connector creates from a garam declaration carries three things: `garam`, the cluster record that applied it, by GRN, and the declaration's identifier and generation. Every other object, whether written with `kubectl`, applied from a file or created by GitOps, is the cluster's. A garam-owned object of another record is neither the cluster's nor this connector's. The connector does not apply over it, delete it or report it.

2. **garam's declarations flow to the cluster.** The connector long-polls garam (ADR 0087) for changes to the declarations placed on its cluster, and applies each one by server-side apply under one field manager. It reverts drift in a garam-owned object: a hand edit is undone at the next apply, which happens on every change and at least every 10 minutes.

3. **Cluster-owned objects flow to garam as reports, and garam does not write them.** The connector reports every object of the declared kinds in the namespaces its release has claimed (decision 8): owner, identity, generation, spec and status. It also reports the outcome of each garam declaration. garam shows a cluster-owned object read-only, with the cluster as its source.

4. **The owner of an object is the only side that changes it, and a collision is refused, never resolved by overwriting.**
   - A garam declaration whose namespace and name are held by a cluster-owned object, or by a garam-owned object of another record, is not applied. The connector reports it as `conflict`, and the object holding the name stays as it is.
   - garam refuses a declaration whose namespace and name its latest report shows held by a cluster-owned object. That refusal is a courtesy, since the report may be stale. The connector's refusal is what holds.
   - Changing an object's owner is not part of this goal. It is an explicit adopt or release action, recorded as an open question in `connector.md`.

5. **A deletion is explicit, never inferred from absence.**
   - garam deletes a garam-owned object by sending a tombstone for its declaration. garam keeps tombstones for 30 days.
   - garam records how far it has purged tombstones. A connector whose cursor lies at or before that point is answered `cursor_expired`, whether or not any tombstone remains, and reads the full set again. A garam-owned object that the full set does not name is reported as `orphaned` and left in place. garam can then send a tombstone for it.
   - A cluster-owned object deleted in the cluster disappears from the next report. garam deletes nothing in the cluster for it.

6. **Losing garam changes nothing in the cluster.** While the connection is down, the connector applies nothing new and deletes nothing. The in-cluster reconciler goes on running every object, whoever owns it.

7. **A declaration carries no secret value and names no container image.** A secret appears only as a reference, `<secret-name>/<key>`, to a Secret in the object's namespace, in the form `internal/secretref` states. Which images run is the chart's configuration. The connector refuses a declaration that carries anything else in those places, and reports it as `invalid` (ADR 0091).

8. **A connector acts only in namespaces its release has claimed.** Each watched namespace holds one claim, a Lease naming one release as `<release-namespace>/<release-name>` (CLAIM-1 to CLAIM-3).
   - **Who writes it.** The chart renders the claim, so `helm uninstall` removes it. Helm refuses to install a second release over a Lease another release owns, so an overlap fails at install, in front of the person installing.
   - **How long it lasts.** It never expires. The programs only read it, and a crashlooping release keeps its namespaces.
   - **Whose identity it carries.** The release's, the same whether the connector is on or off. So the manager needs no GRN to check it, and turning the connector on changes nothing.
   - **What it is.** It coordinates releases the cluster's administrator installed. It is not a security boundary: the record's GRN in each garam-owned object's marker is what keeps one record from changing or reporting another's objects.

## Consequences

- garam holds declarations as data: versioned objects, each with an identifier, a generation and a target cluster and namespace. It reconciles nothing itself. That is garam's half in `interfaces/cluster-connection/v1alpha1/`.
- A GitOps-managed cluster keeps its repository as its source of truth. garam sees what it declares and cannot change it.
- A name reserved in a cluster is unavailable to garam's declarations in that namespace, and the dashboard says why.
- Interface 1 has to define the managed-by marker with a value for garam and a place for the declaration identifier. That is one of the pieces this goal needs from garam-bot.

## Signs the decision was wrong

- Users ask to edit a cluster-owned bot from the dashboard often enough that read-only display is a dead end, and adopting has to become the default rather than an explicit action.
- Orphans pile up after connector outages longer than 30 days, which would show the tombstone window is too short.
- Server-side apply's field ownership and the managed-by marker disagree in a way users meet. An example is a cluster-owned controller adding fields to a garam-owned object, which a revert would then fight.

## Rejected alternatives

- **garam always wins.** A GitOps cluster could not keep its repository as the source of truth, and the cluster would not work without garam, against the 11-21 decision.
- **The cluster always wins.** garam's declarations could be silently overridden, and the dashboard would show a state nobody declared.
- **Last writer wins, by timestamp.** Two sources editing the same object would fight, each undoing the other on every sync, with no owner to resolve it.
- **Deleting what garam's full set no longer names.** One wrong answer from garam would delete the user's running bots. Explicit tombstones bound the damage to what garam explicitly deleted.
