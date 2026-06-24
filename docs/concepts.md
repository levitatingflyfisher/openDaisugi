# Concepts

This document is the "why" — it explains what opendaisugi actually is, in terms
that a skeptical reader can verify against the code. If you want to USE the
library, start with the README quickstart. If you want to EVALUATE it for
adoption, read this first.

## The problem

An LLM proposes an action plan. You want to run it. How do you know the plan
won't do something you didn't ask for?

Prompt-engineering ("please don't rm anything") is not a guarantee. LLM
self-verification ("does this plan look safe?") is the LLM grading its own
homework. What you actually want is a third-party, mechanical proof that the
plan, as written, cannot violate a specific declared policy — a proof the
LLM is not asked to produce or trust.

That proof is the deliverable opendaisugi ships.

## Envelopes

An **Envelope** is the declared policy. Two fields matter:

- **`permissions`** — what kinds of actions are allowed at all (shell commands,
  file reads/writes, network calls, robot joint moves, etc.), with allowlists
  where applicable.
- **`invariants`** — logical predicates that must hold for every step in the
  plan (e.g., "no step's `command` matches `^rm `").

See `src/opendaisugi/models.py` for the exact schema.

Envelopes are data. You can hand-write one, generate one from a task
description via `generate_envelope`, or import one from a pathway bundle.

## The predicate algebra

Invariants are authored in a restricted expression language, not arbitrary
Python. The grammar lives in `src/opendaisugi/predicate.py`; the operators are:

- **Leaves**: `Equals`, `NotEquals`, `Matches`, `NotMatches`, `InSet`,
  `NotInSet`, `NumericRange`, `Exists`, `IsEmpty`
- **Composition**: `And`, `Or`, `Not`, `Implies`
- **Quantification**: `ForallSteps`, `ExistsStep`, `ForallOutputs`
- **Escape hatches**: `LLMCheck` (soft; discharged at Stage 2), `AliasRef`
  (named sub-predicate)

The grammar is deliberately small. Agents can author invariants in this
algebra mechanically (see `tests/fixtures/agent.envelope.yaml` for a
real example), and the grammar keeps them inside what Z3 can reason about.

## Compilation to Z3

`compile_to_z3(expr, scope)` in `src/opendaisugi/predicate_z3.py` walks the
predicate AST and emits real Z3 `BoolRef` expressions over symbolic Z3
`String` and `Real` variables — not `z3.BoolVal` wrappers around
Python-evaluated booleans. This distinction is the thesis: the solver does
the reasoning, not the host language.

Concretely:

- `Matches(path="command", regex=r"^rm ")` compiles to
  `z3.InRe(step.command, translate(r"^rm "))` — Z3's regex primitive over a
  symbolic string.
- `NumericRange(path="velocity_scale", lo=0.0, hi=1.0)` compiles to
  `And(0.0 <= step.velocity_scale, step.velocity_scale <= 1.0)` on a symbolic Real.
- `And`/`Or`/`Not`/`Implies` map to the Z3 operators of the same name.
- `ForallSteps(pred)` unrolls `pred` over each concrete step in the plan
  (plans are bounded, so ∀ becomes a conjunction).

The regex translator (`src/opendaisugi/regex_to_z3.py`) covers a defined
subset of Python's `re` — see [limitations.md](limitations.md) for what's
out.

## Soft nodes

Some predicates cannot be compiled to an exact Z3 formula:

- `LLMCheck` is a natural-language assertion ("this email does not
  impersonate the user"); no Z3 encoding exists.
- Unsupported regex features (lookaround, backreferences, case-insensitive
  flags) fall back to a free Z3 `Bool` rather than silently passing.

Both cases emit a **soft node**: a fresh Z3 `Bool` whose name and kind are
recorded in `CompiledPredicate.soft_nodes`. The solver treats it as a free
variable. Stage-2 verification (see `src/opendaisugi/stage2.py`) discharges
it concretely when the step actually runs — if the LLM check fails or the
regex doesn't match, execution is blocked.

opendaisugi never silently approves a soft node. If a proof depends on one,
the caller sees it.

## Verification stages

Three checkpoints, distinct purposes:

1. **Stage 1 — static plan verification** (`verify(plan, envelope)` in
   `src/opendaisugi/verify.py`). Before any step runs, Z3 proves the
   proposed ActionPlan satisfies all envelope invariants. Returns
   `VerificationResult` with violations, if any.
2. **Stage 2 — per-step output verification** (`verify_completed_step` in
   `src/opendaisugi/stage2.py`). After a step runs, its outputs are checked
   against any soft-node constraints. An email body is inspected at this
   stage for impersonation, for instance — Stage 1 cannot see the body.
3. **Runtime supervision** (`Supervisor` + `RunSession` in
   `src/opendaisugi/supervisor.py` and `run_session.py`). Wraps execution,
   checks permissions per step, records traces to the Journal.

Stage 1 is the SMT proof. Stage 2 is bounded runtime checks. Supervision
is the permission gate at execution time. Each catches a different class
of failure.

## Skills as contracts (v0.11.0)

A **Contract** (`src/opendaisugi/contracts.py`) is an Envelope plus
identity and version metadata — what a skill claims about itself.

`verify_delegation(outer_envelope, skill_contract)` proves whether the
orchestrator can safely delegate to the skill. The proof is **envelope
subsumption** — for every ActionStep the skill's envelope admits, the
orchestrator's envelope must also admit it. Formally:

> outer ⊨ inner  ⟺  UNSAT of `admit_inner(step) ∧ ¬admit_outer(step)`

See `src/opendaisugi/subsumption.py`. When the proof fails, Z3 returns a
concrete `ShellStep` the skill could legally emit that the orchestrator
forbids. That counterexample is not demo theater — it's the model Z3
produced when it could not prove UNSAT.

Subsumption is why an orchestrator can hand work to a LoRA-tuned 1.5B
specialist without trusting the specialist's self-description: the claim
is verified mechanically.

## What this doesn't do

Runtime sandboxing, hallucination detection on free-form LLM output,
cryptographic signature verification (deferred), and many other things
you might reasonably expect from a library with "assurance" in its one-
liner. [limitations.md](limitations.md) is the list.
