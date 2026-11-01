# ADR-0025: A plain install records every call; --enforce is the one opt-in to blocking

- **Status:** Accepted
- **Date:** 2026-10-30

## Context

The gate hook was an opt-in layer. A plain `daisugi install` wrote four layers, and `--gate`
added the hook. On the box that most needed a record of agent tool calls, nobody had passed
`--gate`, so no call was recorded. The owner ruled on 2026-10-28: a plain install records every
call, and blocking is the one opt-in.

Audit mode already exists (ADR-0007). It evaluates each call, logs what enforce would deny, and
always allows. Enforce mode with no registered envelope denies every call.

## Decision

The gate hook is part of every install, in audit mode. `--enforce` installs it in enforce mode
and keeps its envelope check: with no envelope registered, the install refuses before any write.
`--gate` is removed. The old flag is refused with one line that says to drop it.

All three ports (Python, Go, Rust) do the same, and the CLI golden cases prove it.

## Consequences

- Every install records tool calls. `daisugi gate report` has data without a second step.
- Hermes and OpenClaw have no gate hook yet. A plain install on a box with only those shows the
  gate as an honest gap and exits 0.
- Enforce keeps its fail-closed behaviour: the hook ends in `|| exit 2`, and a missing envelope
  stops the install.
- An install from before this change has no gate hook. The owner runs `daisugi install` once more.

## Alternatives considered

- **Keep `--gate` as an alias.** Rejected. There is one real user, and an alias keeps two names for
  one thing.
- **Install enforce by default.** Rejected. With no envelope, enforce denies every call, so the
  agent can do nothing.
