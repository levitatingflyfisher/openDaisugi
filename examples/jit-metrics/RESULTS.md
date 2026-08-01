# JIT-metrics — measured over a real journal corpus

Corpus: `~/.opendaisugi` · 368 interactions (172 succeeded, 196 rejected/failed). Reproduce: `python examples/jit-metrics/measure_corpus.py`.

> **Corpus caveat, stated up front.** This corpus is benchmark- and test-generated: only **2 distinct plan-structure signatures** appear among the successes. The compilable fraction below is therefore a measurement over *this* corpus, not a claim about the diversity of organic agent usage. The ruler is the contribution; point it at your own journal for your own number.

## 1. Guard cost — is checking cheaper than doing?

Z3 `verify` timed over every successful plan/envelope pair. Latency is reported as percentiles, never a mean (a timed-out verify sits at the ceiling and would smear a mean).

| Metric | Value |
|---|---|
| Plans timed | 172 |
| verify latency p50 | **3.05 ms** |
| verify latency p95 | 3.73 ms |
| verify latency max | 13.84 ms |
| Z3 timeout rate | 0% |
| Purely-symbolic envelopes (guard needs **no** LLM) | **100%** |

For the **100%** of envelopes that are purely symbolic, the guard is the measured sub-millisecond Z3 solve above and costs **zero tokens** — orders of magnitude below the LLM call that authored the plan. That is runtime assurance's founding assumption, satisfied and measured. Every envelope in this corpus is symbolic, so the free-guard claim holds for all of it; a corpus whose envelopes carry an `llm_check` soft node would show a lower fraction here, because those pay an LLM at Stage-2 discharge — which is exactly the slice this number sizes.

### Amortization (token estimate, labelled)

The envelope is LLM-authored **once** (the "compile"), then guarded cheaply per action. Using `accounting`'s per-call token *estimates* (compile ≈ 4500 tok, saved per reuse ≈ 4500 tok):

- **break-even ≈ 1.00 reuses** — the compile pays for itself in under one reuse. *(Estimate, not a measurement: the corpus does not store per-call token counts. The latency numbers above are measured; this line is the one modelled figure.)*

## 2. Compilable fraction — how much of the corpus could compile?

Over **all** interactions (the pessimistic denominator — failures and gate-rejections included). A trace is compilable when it lands in a cluster of ≥3 similar successes (the distiller's own hotness precondition) **and** its plan still verifies against its envelope.

| Metric | Value |
|---|---|
| Total interactions (denominator) | 368 |
| Succeeded | 172 (47%) |
| Distillable clusters (≥3) | 1 |
| Traces in distillable clusters | 170 |
| **Compilable (upper bound)** | **170** |
| **Compilable fraction of all interactions** | **46%** |
| Guard-pass rate within hot clusters | 100% |

> **Oracle:** hit × guard_pass — UPPER BOUND: proves authorized + structurally distillable, NOT that the reused plan is correct (no correctness oracle)

The headline is an **upper bound**: it proves each counted interaction is *authorized* (guard passes) and *structurally distillable* (clears the hotness gate), not that a reused plan would be *correct*. A real correctness oracle is a separate research problem; naming the gap is the honest move, and the Stage-4 fidelity benchmark is where the correctness question is attacked empirically.
