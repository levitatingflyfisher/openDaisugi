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
| [0004](0004-layer-not-harness.md) | openDaisugi is a layer; MCP is the control pathway | Superseded in part by ADR-0020 |
| [0005](0005-python-runtime.md) | Stay on Python; Rust only for a profiled bottleneck | Accepted |
| [0006](0006-claude-p-backend.md) | `claude -p` as a keyless LLM backend (stopgap) | Accepted |
| [0007](0007-call-time-gate.md) | A call-time tool gate, shadow-by-default, beside plan verification | Accepted |
| [0008](0008-parameterized-callable-pathways.md) | Distilled pathways become parameterized, callable skills | Accepted |
| [0009](0009-adopt-ruff-format.md) | Adopt Ruff for lint + format | Accepted |
| [0010](0010-compound-shell-decomposition.md) | Opt-in compound-shell decomposition, verify each head | Accepted |
| [0011](0011-verifiable-execution-substrate.md) | A verifiable-execution substrate + cost levers, not "a verifier" | Accepted |
| [0012](0012-gateway-reuse-and-answer-store.md) | Gateway reuse bridges to the pathway engine, plus a freshness-gated answer store | Accepted |
| [0013](0013-unified-save-and-verify-install.md) | One install wires both save (gateway base_url) and verify (gate hook), per-harness | Accepted |
| [0014](0014-envelope-checkable-shell-effects.md) | Redirections, substitutions, and wrappers become envelope-checkable effects | Accepted |
| [0015](0015-cache-aware-sticky-routing.md) | Cache-aware sticky routing, the local rung, and cache visibility | Accepted |
| [0016](0016-evidence-inferred-onboarding-envelopes.md) | Onboarding envelopes are inferred from evidence, not generated from task text | Accepted |
| [0017](0017-lazy-init-and-resident-gate.md) | Lazy package exports and a resident gate: the gate's cost was import, not Z3 | Accepted |
| [0018](0018-configurable-matcher-backend.md) | A configurable, torch-free pathway-reuse embedder (potion) | Accepted |
| [0019](0019-lexical-matcher-zero-model-floor.md) | The `lexical` matcher: a zero-model, zero-download floor | Accepted |
| [0020](0020-layer-floor-loop.md) | The layer, the floor, and the loop | Accepted |
| [0021](0021-int8-matcher.md) | The `int8` matcher: MiniLM without torch, through onnxruntime | Accepted |
| [0022](0022-weave-runner-for-verified-plan-trees.md) | weave: the runner for verified plan trees | Accepted in part (option B) |
| [0023](0023-pando-r2-in-daisugi.md) | Pando R2, the conversation memory, moves from coppice into daisugi | Proposed |
| [0024](0024-commonplace.md) | Commonplace, the owner's notes as semantic memory | Proposed |
| [0025](0025-record-by-default.md) | A plain install records every call; `--enforce` is the one opt-in to blocking | Accepted |
| [0026](0026-names-warden-router-steward-pando.md) | Four parts get new names: warden, router, steward and Pando R2 | Accepted |
| [0027](0027-mcp-argument-rules.md) | Argument rules for MCP tools | Accepted |
| [0028](0028-fenced-warden-records-file-accesses.md) | Inside a fence, the kernel is the file boundary and the warden records file accesses | Accepted |
| [0029](0029-pando-branches.md) | Pando branches: one tree, shaped like an org chart | Accepted |
| [0030](0030-one-default-permission.md) | One default permission per project | Accepted |

## Writing a new one

Copy [`0000-template.md`](0000-template.md) to the next number, fill it in, add a
row above. Keep it to ~one screen — an ADR that needs scrolling is two ADRs.
