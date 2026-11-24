# ADR 0094: Let the operator set how long the connector's key signs before it rotates, 30 days unless set

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

ADR 0088 decision 6 says the connector rotates its key "every 30 days, and on a person's request through the chart". ADR 0093 decision 3 restates the 30 days. The connector built on #440 holds the period as a constant in `internal/clusteridentity`.

The client suite (#439) runs the connector as a binary, reaching it only through its command line and the protocol (`interfaces/cluster-connection/v1alpha1/conformance.md`). Seven of its tests hold ROT-1, ROT-4 and ROT-5, and each needs the connector to rotate during a run. With a fixed 30 days, none of them can run, and #440's ROT-5 criterion, which was left for the client suite, goes unchecked.

The PM decided on 2026-11-24, in the orchestration of #439, that a rotation period is a legitimate setting for whoever runs the connector, not only a test knob.

## Decision

1. **The connector takes `--rotation-period`, a duration, defaulting to 720h.** A key signs that long before the connector rotates it. A deployment that sets nothing rotates every 30 days, as before.
2. **A period of 0 or less is refused at start.** The connector stops with an error naming the flag, rather than rotating without pause or never.
3. **The client command line of `conformance.md` gains an optional `{rotation-period}` placeholder.** The client suite fills it with seconds for its rotation tests and leaves it out everywhere else. A client that takes no such setting has its rotation tests reported as pending, by name.
4. **This supersedes ADR 0088 decision 6, and ADR 0093 decision 3, in the period alone.** Everything else in both decisions stands: the rotation call, the two signatures, the 24-hour rollover, and the request through the chart.

## Consequences

- The connector chart (#445) exposes the period as a value, defaulting to 30 days.
- `docs/architecture/connector.md` states the period as configurable.
- The client suite's rotation tests run against the connector in seconds.

## Signs the decision was wrong

- Deployments set periods short enough that garam's 24-hour rollover holds two live keys most of the time, so ROT-4's 409 is met in normal running.
- A period set in error leaves keys signing far longer than an organization's policy allows, and nothing reports it.

## Rejected alternatives

- **A hidden test-only flag.** A setting that changes behaviour in production is a setting. Hiding it leaves the client suite testing a connector no deployment runs.
- **Triggering a rotation through the protocol or a signal.** The interface has no such operation. A signal is an input the suite cannot send to a client run anywhere but on its own host.
- **Leaving the rotation tests pending.** That leaves ROT-5, the part of rotation that keeps a cluster connected through a lost answer, untested at the binary.
