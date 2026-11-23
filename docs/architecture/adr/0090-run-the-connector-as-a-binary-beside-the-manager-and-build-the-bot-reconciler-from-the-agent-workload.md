# ADR 0090: Run the connector as its own binary beside the manager, and build the Bot reconciler from the Agent workload code

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

This goal's brief asks how this operator's existing reconcile becomes the in-cluster half of the connector, and what is split out.

What the manager runs today (`agent.md`, at `9b3c2dd43e69d815f2fe471f815e29f01cbde109`):

- **The `Agent` reconciler**, `internal/controller`. It builds the StatefulSet of one, the state and workspace claims, the credential Secret's projection, the writer fence (ADR 0042), the PodSecurity `restricted` context and the grace period.
- **The control feed's puller and the recoverer**, `internal/desired`. They exist only where `--control-address` is set, and talk to the control service.
- **The token refresher**, `internal/garam`. It mints each agent's garam tokens under the manager's own key into the agent's credential Secret (ADR 0073).

ADR 0079's seams include "Provisioning stays outside the manager", removed by "an identity it provisions for itself". A connector that registers itself (ADR 0088) provisions its own identity.

The owner decided on 2026-11-24, answering this goal's questions:

- Build the Bot reconciler from the existing Agent workload code.
- The goal depends on garam-bot's interface 1 (`garamsh/garam-bot#1193`) and on a garam-bot runtime image that runs one Bot.
- The control service and the `Agent` kind stay untouched in this goal, and their retirement is decided later.

The 11-21 decision (item 6) makes one pod one PC: one garam-bot runs one organization's bots, with a process per bot.

## Decision

1. **The connector chart deploys two programs, the manager and the connector, as two Deployments from two images.**
   - **The manager reconciles.** It turns interface-1 objects into workloads in the cluster, and it needs no garam connection, key or address to do that. A chart installed with the connector turned off is the standalone cluster mode of the 11-21 decision.
   - **The connector talks to garam.** It holds the cluster's key, registers, rotates, long-polls declarations and writes them as objects, reports, and mints the garam tokens of garam-owned bots.
   - **They meet only through the Kubernetes API.** The connector writes objects and Secrets, and the manager reads them. Neither calls the other, and neither imports the other's domains.

2. **The connector is a new binary, `cmd/connector`.** It is not part of the manager: provisioning its own identity, which ADR 0079 keeps outside the manager, happens there. Like the control service it is a binary that is not the manager, so it follows `stack-go.md` alone (`docs/convention/README.md` §Stack-specific). It reuses `internal/machinetoken` for its exchange, which already sits below both existing binaries.

3. **The Bot reconciler is built from the Agent workload code, not copied from it.** What the two share moves out of `internal/controller` into a package below both reconcilers, named for what it builds and judged under `structure.md` §Shared code. The shared parts are:
   - the StatefulSet of one with its claims;
   - the credential Secret projected to the one container that holds garam tokens;
   - the writer fence;
   - the restricted security context;
   - pull-always, and the grace period.

   The garam-bot pod differs from the Agent pod in one way. It runs one organization's bots of one pool as processes of one runtime, so the Bot objects of a pool are rendered into a file the runtime reads, and each bot's tokens are mounted beside it.

4. **The control-service path is not part of the connector.** `internal/desired` (the control feed's puller and the recoverer), `internal/garam`'s refresher for `Agent`s, the `Agent` kind and the control service stay as they are. The connector chart does not deploy them and they do not run in a user's cluster. Retiring them is a later decision.

5. **ADR 0079's seams are kept, and the connector takes the place that seam asks for.**
   - The manager provisions no identity.
   - Every connection leaves the cluster.
   - garam relays nothing into it.
   - The control service holds no cluster access.

   garam sending declarations is garam acting as the source of desired state, the role the control service holds for `Agent`s. It is not a relay between the manager and another service.

6. **The connector mints a garam-owned bot's tokens under the cluster's identity**, as ADR 0073's refresher does for an agent under the operator's. It writes them to that bot's credential Secret, which the bot's runtime mounts. The bot never holds the cluster's key. The mint route and the bot's identity are garam's bot decision (`garamsh/garam#2027`). Whether a cluster-owned bot gets a garam identity is an open question in `connector.md`.

## Consequences

- A user who wants garam-bot on Kubernetes without garam installs the same chart with the connector off. Nothing in the manager waits on garam.
- The cluster's key sits in a Pod that does not build workloads, and the Pod that builds workloads holds no key. Each of the two holds only the permissions its own work needs (ADR 0091).
- The image count rises to three published programs: manager, control and connector. `delivery.md` gains the connector's image when the chart issue builds it.
- The extraction from `internal/controller` changes the Agent reconciler's code without changing its behaviour. Its envtest suite is what holds that.
- This goal depends on two pieces from garam-bot: interface 1's kinds and managed-by marker, and a runtime image that reads Bot objects from files and runs one process per bot. The goal issue lists them.

## Signs the decision was wrong

- The connector and the manager need to coordinate beyond the Kubernetes API, for example waiting on each other's progress, which would show they are one program.
- The extracted workload package grows a branch per reconciler at every turn. `simplicity.md` reads that as two different things, which should then be kept apart.
- Users run the connector without the manager, which would show the reconciler belongs with something else.

## Rejected alternatives

- **The connector as `Runnable`s inside the manager**, as the puller and the refresher are today. It is one fewer image, but the manager would provision its own identity, against ADR 0079's seam. Its Pod would also hold both the cluster's key and the right to build workloads.
- **Rendering a garam Bot as today's `Agent` kind.** The `sherlock` type it builds is superseded by garam-bot ADR 0031 (`garamsh/garam-bot@542306b0f1ade6e997068e11fcdf92685d40a7e1`), and the owner chose the dependency on garam-bot instead.
- **Making the hosted control service the source of declarations for a connected cluster.** garam owns the dashboard, the bot's identity and its policy (11-21 decisions), so garam is where a declaration is written. A second store would need its own sync.
