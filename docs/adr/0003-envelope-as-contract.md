# ADR-0003: The Envelope is a contract, not config

- **Status:** Accepted
- **Date:** 2026-07-02 (documenting a decision load-bearing since v0.1)

## Context

Something has to name the boundary of what an agent may do. The tempting shape is
a pile of settings scattered across the executors ("this subprocess runner has an
allowlist; that file writer has a root dir"). That shape has no *composition* — you
can't ask "is delegating to this skill safe?" or "is this reused plan within what
the caller allowed?" because there's no single object to reason about.

## Decision

Make the **`Envelope`** (a `Permission` spec + invariants + postconditions +
stakes) the one first-class authorization object, and make **`verify(plan,
envelope)`** the one gate. Everything else is defined in terms of it:

- **Delegation safety** = `envelope_subsumes(caller, contract)` — the same proof,
  applied to two envelopes.
- **Inheritance** (a generated child envelope) must be a proven *tightening* of
  its parent.
- **Reuse / MCP / sub-agents** are all bounded by the *caller's* envelope as the
  authorization ceiling — never their own.

Because it's one object, it composes: containment is transitive, so
"skills-as-contracts," safe sub-agents, and shared pathways all fall out of the
same subsumption proof.

## Consequences

- **Buys:** a single, testable definition of "allowed," and free composition —
  delegation, inheritance, reuse, and swarm deconfliction are all envelope algebra.
- **Costs:** the envelope must model *every* capability dimension (shell, file,
  net, MCP, robot bounds, stakes). When a new effect type is added, the envelope
  and every proof that ranges over it must be extended — an omission there is a
  silent fail-open (this is precisely how the v0.34 inheritance and subsumption
  gaps happened: new capabilities that the proofs didn't yet range over).
- **Forecloses:** per-executor ad-hoc permission config as the source of truth.
  Executors *enforce* re-checks at run time, but the envelope *defines* authority.

## Alternatives considered

- **Per-tool capability config:** rejected — no composition, so no provable
  delegation, which is the whole point.
- **Capability tokens / OS sandboxing only:** complementary, not a replacement —
  they bound the blast radius but can't answer "is this plan within policy?"
  before it runs, and can't reason about LLM-authored predicates.
