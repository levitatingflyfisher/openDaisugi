# ADR-0011: openDaisugi is a verifiable-execution substrate, not "a verifier"

- **Status:** Accepted
- **Date:** 2026-08-06

## Context

openDaisugi began as, and is often described as, "a verifier": an LLM writes a
checkable envelope, a solver proves each action stays inside it before it runs. That
framing is correct for *safety* and undersells the project on *cost*. It left an open
strategic question we could not answer from the inside: is the envelope-and-solver a
**niche** — a safety curiosity — while real token-saving happens elsewhere (caching,
routing, plan reuse)? Or is it the load-bearing piece of a cost story we simply
haven't built yet?

We answered it with a **blind-design gauntlet** — five independent designer agents,
each closed-book (no sight of openDaisugi, no sight of each other), each attacking a
neutral, fingerprint-free version of the same problem, followed by five adversarial
critics and one synthesizer. The full record is in
[docs/exploration/2026-08-blind-design-gauntlet/](../exploration/2026-08-blind-design-gauntlet/).

The result was overwhelming convergence. All five blind architects rebuilt
openDaisugi's spine, and the two things the neutral brief **never hinted at** — a
monotone-narrowing authority lattice (our `envelope_subsumes`) and within-instance
program compilation (our cost ratchet) — came back clean five-for-five. Independent
minds, including a contrarian sent to look *unlike* the field, arrive at the envelope.
It is not a niche; it is canonical. The rare-hard-task worry resolved too: every design
found the same escape — verification amortizes **per-action, not per-repetition**, so
it pays on a task done once.

The synthesizer did not recommend abandoning the envelope, and did not recommend
splitting it into unrelated products. It recommended splitting the one over-claiming
stack **along its currencies** into a safety substrate plus a small family of cost
levers — which is the reframe below, reached independently of us.

## Decision

**openDaisugi is a verifiable-execution *substrate* with a small family of *cost
levers* it underwrites.** The envelope-and-solver is not itself the token-saver; it is
the **trust substrate** that makes the token-savers safe. Because an action is proven
in-bounds, the system can safely compile a batch it otherwise wouldn't, route to a
cheaper model it otherwise couldn't trust, and compact context aggressively because the
policy lives outside the token stream.

The substrate and levers, mapped to what already exists:

1. **The substrate (safety-currency).** The `verify` → `envelope_subsumes` →
   `Supervisor` → journal spine, plus the call-time gate (ADR-0007) and delegation
   (Stage 2). Its product is a correctness guarantee that survives compaction; cost is
   a *bounded by-product*, claimed honestly and small.
2. **Output-collapse lever** — within-instance, blast-radius-proven batch execution
   (the cost ratchet). Honest baseline: **verified vs. unverified script**, not "N
   manual turns" (see below).
3. **$/token lever** — verification-underwritten cheap-model routing: escalate on
   *external* signals (gate rejection, postcondition failure, k-failure stop-loss),
   never the cheap model's self-report. Extends the existing model ladder.
4. **The novel bet — rationale-durable memory.** The one thing none of the five blind
   designs built: externalize the *deliberation* (facts, ruled-out hypotheses,
   promoted invariants), not just post-execution state, so a compacted agent never
   re-derives what it already worked out. This is the only lever that targets the
   irreducible one-off — the modal rare-hard task, which has no repetition to compile.

Three repairs the gauntlet's critics forced, adopted as standing discipline:

- **Two-ledger honest baselines.** The within-instance win is frequency-independent,
  small, and safety-shaped (verified vs. unverified script). Persistence and
  generalization of a distilled pathway are a *separate*, cross-instance win that
  compounds with reuse — but they are exactly the frequency-amortized family the
  hardest requirement said cannot be the *complete* answer. **Kept in separate
  ledgers, both claims hold; merged, they become the over-claim the critics flagged.**
- **Reversibility stays a layer concern, not a harness takeover.** All five designs
  reached for copy-on-write rollback; ours has none. But *owning* workspace snapshots
  is state management, which [ADR-0004](0004-layer-not-harness.md) forecloses. The
  in-scope form is an **external deed ledger**: the layer *records* each reversible
  deed (extending the per-step `Receipt` the Supervisor already writes) so the
  *harness* can roll back. Layer records; harness reverts.
- **Escalation and authority key on external signals**, never a model grading its own
  output — already true of the gate, now explicit for routing.

## Consequences

- **Buys:** an honest, defensible story for cost that keeps the safety guarantee
  central rather than bolting cost on beside it; a validated claim (five independent
  architects) that the core is canonical, not niche; a clear frontier to own
  (rationale-durable memory) that the field has not built; and a way to talk about
  savings on rarely-repeated hard tasks without over-claiming.
- **Costs:** the honest baseline shrinks some headline cost numbers to "safety-shaped"
  — a real benefit that resists being sold as a large multiplier. We accept the
  smaller-but-true number over the larger-but-wrong one, consistent with the project's
  scorecard ethos.
- **Forecloses:** selling the envelope as, by itself, a token-saving product; and
  owning workspace state/rollback (still barred by ADR-0004 — the deed ledger is the
  compliant substitute).

## Alternatives considered

- **Abandon the envelope, chase cost directly** (caching/routing/reuse without a
  gate): rejected — the blind evidence is that competent designers rebuild the gate
  precisely *because* it is what makes aggressive cost-cutting safe; without it the
  savers are reckless.
- **Split into two unrelated products** (a safety/robotics verifier; a cost DAG):
  rejected — the synthesizer's split is along *currencies within one composable
  stack*, not into unrelated products; the substrate underwrites every lever, so
  severing them removes the thing that makes the levers defensible.
- **Adopt copy-on-write rollback wholesale**: rejected on ADR-0004 grounds — replaced
  by the external deed ledger, which achieves reversibility while the layer stays a
  layer.

## Provenance

The convergence experiment that grounds this decision was run as an automated
multi-agent workflow and is recorded, per the workshop convention, as an accurate
account of what the exercise produced — not a proof. The value is in the method and
the convergence, not in any model that ran it. Full material:
[docs/exploration/2026-08-blind-design-gauntlet/](../exploration/2026-08-blind-design-gauntlet/).

**On names.** The blind synthesizer, working in its own vocabulary, called the four
bets *Bailiff* (the substrate), *Foreman* (batch compilation), *Underwriter*
(routing), and *Palimpsest* (rationale-durable memory). openDaisugi keeps its own
idiom — **the substrate, the cost ratchet, gate-underwritten routing, and the rationale
ledger** — and treats those coined names as the record of the experiment, not product
names. The one new artifact this ADR introduces, the **deed ledger**, is deliberately a
sibling of the existing journal: where the journal's `Receipt` records that a step
*happened*, the deed ledger records how to *undo* it; the rationale ledger, later, will
record what was *reasoned*.
