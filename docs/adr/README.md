# Architecture Decision Records

An ADR captures **one architectural decision**: the context that forced it, the
choice made, and the consequences we accepted. They are immutable once accepted —
if a decision is revisited, add a *new* ADR that supersedes the old one (and mark
the old one `Superseded by ADR-NNNN`) rather than editing history.

Read these when you're about to change something load-bearing and want to know
whether you're fixing a mistake or unknowingly reopening a settled trade-off.

## Index

| # | Decision | Status |
|---|---|---|
| [0001](0001-fail-closed-default.md) | Fail-closed is the default posture | Accepted |
| [0002](0002-z3-over-heuristics.md) | Z3 / SMT for verification, not heuristics | Accepted |
| [0003](0003-envelope-as-contract.md) | The Envelope is a contract, not config | Accepted |
| [0004](0004-layer-not-harness.md) | openDaisugi is a layer; MCP is the control pathway | Accepted |
| [0005](0005-python-runtime.md) | Stay on Python; Rust only for a profiled bottleneck | Accepted |
| [0006](0006-claude-p-backend.md) | `claude -p` as a keyless LLM backend (stopgap) | Accepted |
| [0007](0007-call-time-gate.md) | A call-time tool gate, shadow-by-default, beside plan verification | Accepted |
| [0007](0007-call-time-gate.md) | A call-time tool gate, shadow-by-default, beside plan verification | Accepted |

## Writing a new one

Copy [`0000-template.md`](0000-template.md) to the next number, fill it in, add a
row above. Keep it to ~one screen — an ADR that needs scrolling is two ADRs.
