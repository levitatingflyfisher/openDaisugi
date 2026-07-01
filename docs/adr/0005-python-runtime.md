# ADR-0005: Stay on Python; Rust only for a profiled bottleneck

- **Status:** Accepted
- **Date:** 2026-07-02

## Context

A recurring, reasonable question: should the core move to Rust (or Haskell) for
speed, memory, safety, and single-binary distribution? The codebase is ~20k LOC
across 68 modules with 1598 tests. The instinct is worth taking seriously — and
worth deciding on the record so it stops recurring.

## Decision

**Stay on Python for the core.** Reach for Rust only in two narrow, additive ways:
(1) a **PyO3 extension for a specific hot path** *after* profiling proves it's a
bottleneck, or (2) a **single-binary distribution shell** if standalone/local-LLM
packaging demands it. No rewrite.

## Consequences

- **Buys:** we keep 1598 tests, the whole LLM ecosystem (litellm, instructor,
  pydantic, the MCP SDK, sentence-transformers), and the `z3-solver` binding — none
  of which have first-class Rust/Haskell equivalents. Velocity stays high.
- **Costs:** Python's startup, GIL, and dependency-environment friction remain.
  Standalone distribution needs a packaging answer (PyInstaller/shiv/PyOxidizer or
  a launcher), not a free static binary.
- **Forecloses:** nothing permanently — a hot-path Rust extension or a distribution
  shell is compatible with this decision. A *rewrite* is what's ruled out.

## Alternatives considered

- **Rust rewrite:** rejected. The actual hot path is Z3 (native C++ regardless of
  host language), so the speed argument doesn't touch the bottleneck; against that,
  a rewrite discards the test suite and the Python-native LLM ecosystem and would
  reintroduce the exact bug classes the v0.34 campaign just closed. The one real
  Rust advantage — single-binary distribution — is a packaging problem, solvable
  without a rewrite.
- **Haskell rewrite:** rejected. `sbv` gives elegant SMT and the type system could
  encode parts of the permission algebra, but the LLM/embedding ecosystem is even
  weaker and team velocity would crater. Beautiful, wrong tool here.
- **Python + mandatory Rust extensions everywhere:** rejected as premature —
  optimize where a profiler points, not speculatively.
