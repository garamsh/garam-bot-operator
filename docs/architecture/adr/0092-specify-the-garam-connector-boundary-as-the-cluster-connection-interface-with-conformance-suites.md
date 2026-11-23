# ADR 0092: Specify the garam-to-connector boundary as the cluster-connection interface, with conformance suites and a reference server

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The owner's principle of 2026-11-21 is interfaces first (`garamsh/discussions` discussion 6, comment 19791278): every component boundary is a versioned public interface with conformance tests, and has more than one implementation from the start. `garamsh/convention-driven-project` states it as `docs/convention/interfaces.md` (#594, read at `aea0a7d222f693855f47188df5e11e47f2af5d62`). Under §Rules it asks for four things:

- The boundary is a versioned public interface.
- It has a conformance test that runs against any implementation through the interface alone.
- It has more than one implementation from the start, and in pre-stable releases a reference implementation counts as the second.
- Its specification states its compatibility rules.

This repository has not adopted that file into `docs/convention/`. The owner directed this goal to follow it for the garam-to-connector boundary.

The boundary crosses two repositories and two release cycles. garam implements the server and this repository implements the client.

garam-bot's interface 4, the control connection protocol (`garamsh/garam-bot#1193`), is garam to the garam-bot process: bot tokens, policy and messages. The owner decided on 2026-11-24 that interface 4 stays separate from this one, and that this one lives here, under `interfaces/cluster-connection/v1alpha1/`.

## Decision

1. **The boundary is the interface `cluster-connection`, version `v1alpha1`, at `interfaces/cluster-connection/v1alpha1/`.**
   - `README.md` is normative for behaviour, with every requirement named.
   - `openapi.yaml` is normative for shapes.
   - `conformance.md` maps every requirement to a named test.

   A consumer depends on the version and not on either implementation. The path's version is the interface's version.

2. **There are two suites, server and client.** Each reaches an implementation through the protocol and a small fixture only.
   - The server suite runs against any server's URL, so garam runs it in its own CI against its own build.
   - The client suite runs a connector against the reference server and a Kubernetes API server.

   This is the shape of `csi-sanity` against a CSI driver, which interfaces.md gives as its example.

3. **The reference server is an in-memory implementation in this repository.** It is the second server implementation `v1alpha1` needs. It is also what the client suite and this repository's end-to-end test run against, so the connector is tested without a garam deployment. The server suite's own client is the second client.

4. **garam implements the server, and owes what the interface does not carry.** garam's half is written in `connector.md` §What garam owes as obligations on garam, for garam's repository to decide and build:
   - the Cluster resource and its browser API;
   - the registration token;
   - the IAM actions;
   - placing a declaration on a cluster;
   - showing reports;
   - the bot-token mint for a cluster.

   The new garam ADR that brings back the one-time token (ADR 0088) is garam's.

5. **`v1` needs two production implementations of each side.** `v1alpha1` is pre-stable, so the reference implementations stand in until then, as interfaces.md allows.

6. **Interface 1 is a dependency, not part of this interface.** Its kinds travel as objects whose `spec` this protocol does not restate. A new version of interface 1 makes a new version of this one only where the set of kinds changes.

## Consequences

- garam and this repository can build their halves at the same time, each against the reference and the suites, and meet at the end.
- A change to the wire is a change to `interfaces/cluster-connection/`, reviewed here, with the suites updated in the same pull request.
- `interfaces/` is a new top-level folder beside the kubebuilder scaffold's. It is not a domain of either binary (`structure.md`).
- Adopting interfaces.md into `docs/convention/` is the PM's. Until it is adopted, this ADR is what binds this boundary to it.

## Signs the decision was wrong

- garam cannot run the server suite in its CI without this repository's toolchain, so the suite is run by nobody but us.
- The reference server diverges from garam's behaviour in ways the suite does not catch, and the connector passes against the reference and fails against garam.
- Interface 4 and this interface carry the same thing twice, such as both minting bot tokens, which would show they were one interface.

## Rejected alternatives

- **Writing the wire into garam's `machine.yaml` alone.** garam's specification would then be the boundary, shaped by its one implementation, with no conformance suite another implementation could run.
- **Folding this into garam-bot's interface 4.** Owner's decision above. Interface 4 is a bot runtime's connection, and this is a cluster's.
- **A conformance suite with no reference server.** The client suite would need a garam deployment, and `v1alpha1` would have one server implementation.
