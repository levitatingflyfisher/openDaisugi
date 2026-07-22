# ADR-0007: A call-time tool gate, shadow-by-default, beside plan verification

- **Status:** Accepted
- **Date:** 2026-07-08

## Context

Plan-time verification covers only plans the library is *handed*. An agent
already running in a host harness improvises tool calls with no plan artifact —
there is nothing to verify before the run starts. The repo's `pre_tool_call`
hook observes those calls but is passive and fails **open** by design ("never
hang the host"), which is correct for capture and wrong for protection.
Meanwhile delegated sub-agents are pure-reasoning leaves (`TaskStep` carries no
capability fields), so "a sub-agent that can act, provably bounded" is not
expressible at all. The landscape at decision time: shipped tool-call guards
(host permission patterns, policy engines, injection classifiers) do pattern or
policy matching; solver-backed per-call gating with subsumption reasoning ships
nowhere we could find. The mechanism was validated academically with manual SMT
encoding named as its bottleneck — the restricted predicate algebra is this
project's existing answer to exactly that bottleneck.

## Decision

Build call-time enforcement as a **second checkpoint beside plan verification,
never a replacement for it**:

1. **The gate.** The hook seam gains an enforce mode: each intercepted tool
   call is synthesized into a one-step plan and verified against the session's
   registered envelope before it runs. Deny by default: unknown tool,
   unparseable input, internal exception, or slow verifier all deny. The gate
   owns an inner timeout that itself denies, because every known host's outer
   hook timeout fails open. The matcher is total (`*`); classification happens
   inside the gate. Capture mode keeps its fail-open contract — the two modes
   share a seam, not a failure policy.
2. **Shadow by default.** First run observes and reports what it *would* have
   denied; one flag flips to enforce. Shadow mode is documented as observation,
   not protection. (Minority position, recorded: enforce-by-default with a
   `--shadow` flag. Overruled for adoption reasons, not safety ones — a gate
   nobody dares install protects nobody.)
3. **A new step type for tool-using sub-agents.** `AgenticStep` — registered
   with its own permission-checking arm, never riding TaskStep's
   pure-reasoning exemption. Enforcement is defense in depth: a static tool
   allowlist derived from the envelope, plus the gate wired into the
   sub-agent's own hook config, supplied from outside anything the sub-agent
   can write.
4. **The adversarial suite is the merge gate.** No gate code merges to master
   until the suite passes — attack denial and benign false-positive rates
   both measured, corpus not solely self-authored.

## Consequences

- **What this buys:** enforcement for agents we didn't author and plans that
  don't exist; the delegation ban becomes a delegation *boundary*; the hook
  seam stops being observation-only.
- **What this costs — stated, not buried:** a call-time gate enforces *safety
  properties only*. It cannot establish liveness or plan-structure properties
  ("eventually returns to base", ordering, completeness) — those remain
  plan-verification territory. A call being inside the envelope does not make
  the trajectory benign; individually-admitted calls can compose into harm.
  The yellow paper gains a two-checkpoint section stating this precisely.
- **Fail-open edges that remain outside our control**, named in the public
  scorecard: the host's outer hook timeout; a harness that silently stops
  firing hooks; a host whose block path doesn't block (per-version contract
  tests, Stage 5 of the roadmap, are the mitigation).
- The host contract is pinned by a committed live test
  (`tests/test_hook_gate_contract.py`): settings-injected PreToolUse hooks
  fire in headless runs and an exit-2 deny blocks the call. Measured full
  Python-side round trip ~0.55 s steady state, dominated by package import —
  a lean gate entry module is the optimization seam; a resident process is
  not required for v1.
- An over-denying gate must be disarmable by one command that does not itself
  require an allowed tool call.

## Alternatives considered

- **HTTP-daemon hook (resident verifier).** Rejected: connection failure or
  timeout is a non-blocking error on the host side — structurally fail-open.
  Reopens only with a transport whose failure denies.
- **Extending TaskStep with tool fields.** Rejected: TaskStep's tool-lessness
  is a load-bearing guarantee (no delegated-output→command splice, no
  disk/shell/network from a confused sub-model). A new type keeps the old
  proof intact.
- **Static `--allowedTools` alone (no live gate).** Rejected as the primary
  mechanism: string patterns, not proof, and invisible to the envelope's
  stakes/strictness. Kept as defense in depth.
- **Plan-verification only (status quo).** Rejected: leaves the flagship
  use case — bounding an agent you're already running — permanently out of
  scope, while the seam and the verifier both already exist.
