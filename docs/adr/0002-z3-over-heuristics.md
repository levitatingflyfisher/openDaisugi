# ADR-0002: Z3 / SMT for verification, not heuristics

- **Status:** Accepted
- **Date:** 2026-07-02 (documenting a decision load-bearing since v0.11)

## Context

The central claims — "this plan can only do what this envelope permits,"
"this delegation is contained," "this trajectory stays in bounds" — are claims
about *all possible* behaviors admitted by a spec, not about a sampled few. A
string-match or example-based check answers "did I find a violation in the cases I
looked at?" The question we actually need answered is "does a violating case
*exist*?" Those are different questions, and only the second one is safe.

## Decision

Compile permission/predicate/geometry constraints to **SMT (Z3)** and decide them
by solving, not by pattern-matching. The verifier translates globs and regexes to
symbolic string/automaton constraints (`regex_to_z3.py`), permission scopes and
subsumption to existential witness searches (`subsumption.py`), predicate-algebra
invariants to logical formulae (`predicate_z3.py`), and robotics bounds to
arithmetic constraints (`z3_checks.py`). A Z3 `unknown` or timeout is treated as
*not proven* (see ADR-0001).

## Consequences

- **Buys:** soundness. "Subsumes" means *no admitted counterexample exists*, found
  by search rather than asserted by inspection. Constructs that would slip past a
  regex (word boundaries, clustered shell flags, symlink escapes) are caught
  because the proof reasons about the whole space.
- **Costs:** latency and complexity. Z3 solves take milliseconds-to-seconds and
  need timeouts; unsupported constructs must degrade to fail-closed rather than
  silently to fail-open. The translation layer (`regex_to_z3`, glob→Z3) is subtle
  and is itself a place bugs hide.
- **Forecloses:** a "just grep the command" fast path in the core. Cheap
  set/string pre-checks run *first* (Stage 1) to short-circuit, but they never
  *replace* the SMT proof for the claims that need it.

## Alternatives considered

- **Allowlist/denylist string matching:** rejected — unsound by construction; the
  v0.14 shell-interpreter and v0.34 clustered-flag findings are exactly what this
  misses.
- **Property-based / fuzz testing of plans:** useful as a *test* technique, not as
  the runtime gate — it samples, it doesn't prove, and it can't run in the
  millisecond budget of a per-step check.
