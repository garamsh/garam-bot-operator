# ADR 0087: Connect a user's cluster to garam through a connector it installs, which dials garam over HTTPS long-poll

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

`garamsh/naru#373` §3 records the decision of 2026-11-24 (`garamsh/discussions` discussion 6, comment 19835113): a user connects a cluster they own, EKS or any conformant cluster, by installing a garam connector with a Helm chart. The connector dials out to garam, registers with a one-time token, and then receives the user's declarations of bots and workspaces. That decision also says this matches the decision of 2026-11-21 (comment 19791278): bots and workspaces are declared from a cluster CRD, a notebook file or the dashboard, and they work without garam. On 2026-11-24 the owner resumed this repository for that goal (relayed by cupa to this goal's dispatch).

[ADR 0079](0079-run-only-on-our-own-system-and-keep-the-seams-that-allow-a-bring-your-own-operator.md) decided that this project runs only on our own system, with BYO operator on hold, and kept the seams that would let someone else run the manager. The connector is BYO: a user installs it in a cluster garam does not run.

Three transports were weighed for the connection. All three are outbound from the cluster.

| Transport | Precedent | Against it here |
|---|---|---|
| HTTPS long-poll for what garam sends, plain POST for what the cluster reports | HCP Terraform agents make only outbound TCP connections, to app.terraform.io on tcp/443 for "Polling for new workloads" (developer.hashicorp.com/terraform/cloud-docs/agents/requirements, read 2026-11-24). garam's own message claim is a long poll that any instance answers (garam ADR-0013, `garamsh/garam@6c3aaf69ed5a76bf4451353371f99a9d2a390222` `docs/architecture/adr/0013-agent-messaging.md`). Open Cluster Management's klusterlet has its prescriptions "pulled from the hub instead of pushing" (open-cluster-management.io/docs/concepts/architecture, read 2026-11-24). This repository's own control-service feed is a cursor that "only wakes a long poll of up to 30 seconds" ([ADR 0043](0043-pull-managed-agents-desired-state-from-the-control-service-and-keep-each-grn-on-one-source.md), contract C2 of ADR 0040). | Latency is a poll's: a change waits for the open request to be answered. That is bounded by the request being answered at once when a change is written. |
| gRPC bidirectional stream | — | It needs HTTP/2 end to end, which corporate proxies and some load balancers break. A stream is held by one replica, so a change written on another has to be routed to it. garam's machine listener serves OpenAPI over HTTP/1.1 (garam ADR-0019), so this would be a second protocol stack there. |
| WebSocket tunnel | Rancher's cluster agent "opens a tunnel to the corresponding cluster controller within the Rancher server", and Rancher's authentication proxy "forwards all Kubernetes API calls to downstream clusters" (ranchermanager.docs.rancher.com, Communicating with downstream user clusters, read 2026-11-24) | It needs the same sticky replica as a stream. The tunnel shape also gives the server a path into the cluster, which this design refuses (ADR 0091). |

## Decision

1. **A user connects a cluster by installing one Helm chart, the connector chart, in it.** It runs in EKS or any conformant cluster and needs nothing garam runs inside that cluster. `connector.md` describes it. The chart is the product a user installs, so ADR 0079's decisions 1 and 2 are superseded in part: this project is no longer only for our own system, and BYO is no longer on hold, for the connector. What the chart deploys is ADR 0090's.

2. **Every connection is opened by the connector, outbound, over HTTPS to one garam address.** garam never dials the cluster, no port of the cluster is opened for garam, and nothing relays a connection into it.

3. **The transport is HTTPS long-poll for what garam sends, and a plain POST for what the connector reports.** The connector holds one open GET for declarations. garam answers it as soon as a change is written, and otherwise after a wait of at most 50 seconds, and the connector opens the next one. Reports are POSTs. Any garam replica answers any request: the state a request needs is read from garam's store, not from the connection. The wire is `interfaces/cluster-connection/v1alpha1/` (ADR 0092).

4. **The cluster works without garam.** A lost connection stops nothing already declared: the connector keeps what it applied and retries, and the in-cluster reconciler does not depend on the connector at all (ADR 0090).

## Consequences

- ADR 0079's status line names this ADR. Its seams stand and now carry a deployment that crosses a cluster boundary: ADR 0090 places the connector so that none of them is removed.
- garam carries a new resource, the cluster, and a connection service on its side. ADR 0088 and ADR 0092 state what garam owes.
- A connection through a forward proxy works wherever HTTPS does, and the chart takes a proxy address as a value.
- A declaration reaches the cluster within one round trip of being written, not within a fixed polling interval, provided garam's implementation wakes open polls on a write. The conformance suite holds it to that.

## Signs the decision was wrong

- Users' networks refuse long-held HTTPS requests often enough that the 50-second wait has to drop to a few seconds, which turns long-poll into frequent polling.
- garam cannot wake an open poll on another replica without a shared broker, and the broker costs more than a stream would have.
- A use arrives that needs garam to call into the cluster synchronously, such as reading a pod log on demand, and it cannot be met by a request the connector picks up.

## Rejected alternatives

- **gRPC stream.** The table above.
- **WebSocket tunnel.** The table above.
- **Short polling at a fixed interval.** For declarations a user waits on, it trades latency against load, and long-poll has neither cost.
- **garam reaching the cluster's API with a credential the user grants.** That needs an inbound path or a held API credential, which ADR 0091 refuses.
