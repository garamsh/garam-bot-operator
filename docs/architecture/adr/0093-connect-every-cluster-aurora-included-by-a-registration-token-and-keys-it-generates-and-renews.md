# ADR 0093: Connect every cluster, aurora included, by a registration token given once and keys it generates and renews itself

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The owner's direction of 2026-11-24 (`garamsh/discussions` discussion 6, comment 19852665, relayed by the PM on #447) sets the rule for goal 4 (#436). It says, in English:

- Today a person issues an operator key in garam's console for each process, the manager and the control service. The key is a PEM, a client id and a token URL, and the person places it into a cluster Secret by hand (`configuration.md`, the row naming `control.operatorKey` and `operator.key`; ADR 0072).
- What the owner wants: a registration token taken once from the console and given at install. With it the cluster registers itself, makes its own key and renews it. No person carries a PEM. The precedents named are GitHub Actions runners and Actions Runner Controller, and the Rancher agent.
- Goal 4's connection works this way, and so does this project's own cluster, aurora.
- The number of processes, the key's lifetime and how it is renewed are this project's to design.

ADR 0088 already decides that the connector works this way. No ADR states it as the rule for every cluster, and none names aurora.

This project once had a one-time-token path for the manager, and removed it. Under ADR 0020 to ADR 0022 the manager enrolled with a token and obtained a client certificate. ADR 0072 removed that enrolment when garam replaced client certificates with keys under `machine-token.v2` (garam ADR-0102), and ADR 0068 keeps no second path beside the one in use. That ground was the credential's kind, not registration. ADR 0088's registration ends in a key under `machine-token.v2`, generated and renewed in the cluster under the cluster-connection interface, so the reason for the removal does not reach it.

## Decision

1. **Every path this project adds for a cluster to reach garam starts from a registration token, given once at install.** On such a path no private key is issued by garam, carried by a person, or placed into any process by hand. The one standing exception is the issued keys of ADR 0072 decision 1, for the manager and the control service, until an ADR supersedes that decision (decision 5).
2. **The key is generated in the process that holds it.** It is kept in that process and in one Secret in its release namespace, and nowhere else (ADR 0088 decisions 3 and 4, ADR 0091 decision 5).
3. **The cluster renews its own key, without a person.** The connector does it by ROT-1 to ROT-5 every 30 days (ADR 0088 decision 6). A lost key is replaced by registering again with a new token (ADR 0088 decision 7).
4. **aurora connects by the same path**, through the connector chart and a cluster record of its own, and not by a path of its own.
5. **The manager's and the control service's issued keys do not meet this rule yet.** Today aurora places both by hand under ADR 0072. They stay in place until a later ADR decides how those two processes reach garam under decision 1, so a running deployment is never left without a key. This ADR does not decide that, and supersedes nothing.

## Consequences

- Every path this project adds for a cluster to reach garam is judged against decisions 1 to 3. One that needs a hand-placed key is refused.
- aurora's move to the connector path needs the decision that decision 5 leaves open. Until it lands, `configuration.md` describes the issued keys aurora places today, and that description is accurate.
- The client suite (#439) and the reference server (#447) test the only connection path this rule admits.

## Signs the decision was wrong

- A cluster cannot reach garam's registration route at install, for example behind a proxy that refuses the unauthenticated call. Only a key placed out of band then connects it.
- A process cannot hold a Secret of its own in its release namespace, so a generated key has nowhere to live but the process.

## Rejected alternatives

- **Keep issued PEMs for this project's own cluster and use registration only for users' clusters.** The owner's direction names aurora, and two paths would mean two sets of credential handling to keep and test.
- **Move the manager and the control service to registration in this ADR.** How many processes register, and under which records, depends on the connector's shape, and that is still being built (#440 to #445). Deciding it now would decide a process count before the program it counts exists.
